// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package diagnostics

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl/licenseccl"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/diagnostics/diagnosticspb"
	"github.com/cockroachdb/cockroach/pkg/server/status/statuspb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descbuilder"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logcrash"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/mitchellh/reflectwalk"
	"google.golang.org/protobuf/proto"
)

// TelemetryHttpTimeout allows for configuration of the client timeout
// for sending telemetry reports. It is not expected that customers
// would tweak this.
var TelemetryHttpTimeout = envutil.EnvOrDefaultDuration("COCKROACH_TELEMETRY_HTTP_CLIENT_TIMEOUT", 1*time.Minute)

// NodeStatusGenerator abstracts the status.MetricRecorder for read access.
type NodeStatusGenerator interface {

	// GenerateNodeStatus returns a status summary message for the node. The summary
	// includes the recent values of metrics for both the node and all of its
	// component stores. When the node isn't initialized yet, nil is returned.
	GenerateNodeStatus(ctx context.Context) *statuspb.NodeStatus
}

var reportFrequency = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"diagnostics.reporting.interval",
	"interval at which diagnostics data should be reported",
	time.Hour,
	settings.WithPublic)

// Reporter is a helper struct that phones home to report usage and diagnostics.
type Reporter struct {
	StartTime  time.Time
	AmbientCtx *log.AmbientContext
	Config     *base.Config
	Settings   *cluster.Settings

	// StorageClusterID is the cluster ID of the underlying storage
	// cluster. It is not yet available at the time the reporter is
	// created, so instead initialize with a function that gets it
	// dynamically.
	StorageClusterID func() uuid.UUID
	TenantID         roachpb.TenantID
	// LogicalClusterID is the tenant-specific logical cluster ID.
	LogicalClusterID func() uuid.UUID

	// SQLInstanceID is not yet available at the time the reporter is created,
	// so instead initialize with a function that gets it dynamically.
	SQLInstanceID func() base.SQLInstanceID
	SQLServer     *sql.Server
	InternalExec  *sql.InternalExecutor
	DB            *kv.DB
	Recorder      NodeStatusGenerator

	// Locality is a description of the topography of the server.
	Locality roachpb.Locality

	// TestingKnobs is used for internal test controls only.
	TestingKnobs *TestingKnobs

	// LastSuccessfulTelemetryPing records the current timestamp in
	// seconds since the Unix epoch whenever we successfully make contact
	// with the registration server. This timestamp will be updated
	// regardless of whether the response we get back is successful or
	// not.
	LastSuccessfulTelemetryPing atomic.Int64

	// client is the HTTP client used for sending requests to the
	// registration server.
	client *httputil.Client
}

func NewDiagnosticReporter(
	startTime time.Time,
	ambientCtx *log.AmbientContext,
	config *base.Config,
	settings *cluster.Settings,
	storageClusterID func() uuid.UUID,
	logicalClusterID func() uuid.UUID,
	tenantID roachpb.TenantID,
	sqlInstanceID func() base.SQLInstanceID,
	sqlServer *sql.Server,
	internalExec *sql.InternalExecutor,
	db *kv.DB,
	recorder NodeStatusGenerator,
	locality roachpb.Locality,
) *Reporter {
	timeout := TelemetryHttpTimeout
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	} else if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}

	r := &Reporter{
		StartTime:        startTime,
		AmbientCtx:       ambientCtx,
		Config:           config,
		Settings:         settings,
		StorageClusterID: storageClusterID,
		LogicalClusterID: logicalClusterID,
		TenantID:         tenantID,
		SQLInstanceID:    sqlInstanceID,
		SQLServer:        sqlServer,
		InternalExec:     internalExec,
		DB:               db,
		Recorder:         recorder,
		Locality:         locality,
		client:           httputil.NewClientWithTimeout(timeout),
	}
	r.LastSuccessfulTelemetryPing.Store(r.now().Unix())

	return r
}

// shouldReportDiagnostics determines using the diagnostics report setting in
// addition to the license value to determine whether to send telemetry data.
// If the reporting value is true, or the cluster is on a Trial or Free license
// it returns true.
func shouldReportDiagnostics(ctx context.Context, st *cluster.Settings) bool {
	if logcrash.DiagnosticsReportingEnabled.Get(&st.SV) {
		return true
	}

	license, err := utilccl.GetLicense(st)
	// If we cannot fetch the license, we do not send the report.
	if err != nil {
		log.Errorf(ctx, "error fetching license in shouldReportDiagnostics: %s", err)
		return false
	}
	if license == nil {
		return false
	}
	isLimited := license.Type == licenseccl.License_Free || license.Type == licenseccl.License_Trial

	return isLimited
}

// PeriodicallyReportDiagnostics starts a background worker that periodically
// phones home to report usage and diagnostics.
func (r *Reporter) PeriodicallyReportDiagnostics(ctx context.Context, stopper *stop.Stopper) {
	_ = stopper.RunAsyncTaskEx(ctx, stop.TaskOpts{TaskName: "diagnostics", SpanOpt: stop.SterileRootSpan}, func(ctx context.Context) {
		var cancel context.CancelFunc
		ctx, cancel = stopper.WithCancelOnQuiesce(ctx)
		defer cancel()
		defer logcrash.RecoverAndReportNonfatalPanic(ctx, &r.Settings.SV)
		nextReport := r.StartTime

		var timer timeutil.Timer
		defer timer.Stop()
		for {
			// TODO(dt): we should allow tuning the reset and report intervals separately.
			// Consider something like rand.Float() > resetFreq/reportFreq here to sample
			// stat reset periods for reporting.
			if shouldReportDiagnostics(ctx, r.Settings) {
				r.ReportDiagnostics(ctx)
			}

			nextReport = nextReport.Add(reportFrequency.Get(&r.Settings.SV))

			timer.Reset(addJitter(nextReport.Sub(timeutil.Now())))
			select {
			case <-stopper.ShouldQuiesce():
				return
			case <-timer.C:
			}
		}
	})
}

func (r *Reporter) now() time.Time {
	if r.TestingKnobs != nil && r.TestingKnobs.TimeSource != nil {
		return r.TestingKnobs.TimeSource.Now()
	}
	return timeutil.Now()
}

// ReportDiagnostics phones home to report usage and diagnostics.
//
// NOTE: This can be slow because of cloud detection; use cloudinfo.Disable() in
// tests to avoid that.
func (r *Reporter) ReportDiagnostics(ctx context.Context) {
	ctx, span := r.AmbientCtx.AnnotateCtxWithSpan(ctx, "usageReport")
	defer span.Finish()

	report := r.CreateReport(ctx, telemetry.ResetCounts)

	license, err := utilccl.GetLicense(r.Settings)
	if err != nil {
		if log.V(2) {
			log.Warningf(ctx, "failed to retrieve license while reporting diagnostics: %v", err)
		}
	}
	url := r.buildReportingURL(report, license)
	if url == nil {
		return
	}

	b, err := protoutil.Marshal(report)
	if err != nil {
		log.Warningf(ctx, "%v", err)
		return
	}

	res, err := r.client.Post(
		ctx, url.String(), "application/x-protobuf", bytes.NewReader(b),
	)
	if err != nil {
		if log.V(2) {
			// This is probably going to be relatively common in production
			// environments where network access is usually curtailed.
			log.Warningf(ctx, "failed to report node usage metrics: %v", err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// We consider timeout errors to signal successful "contact" with
			// telemetry server. They can happen for a number of reasons
			// outside of the cluster's control.
			r.LastSuccessfulTelemetryPing.Store(r.now().Unix())
		}
		return
	}
	defer res.Body.Close()
	b, err = io.ReadAll(res.Body)
	if err != nil {
		log.Warningf(ctx, "failed to report node usage metrics: status: %s, body: %s, "+
			"error: %v", res.Status, b, err)
		return
	}

	// If `err` == nil then we assume that we've made successful contact
	// with the telemetry server and any further problems are not the
	// customer's fault. We update the telemetry timestamp before moving
	// on with other request handling.
	r.LastSuccessfulTelemetryPing.Store(r.now().Unix())

	if res.StatusCode != http.StatusOK {
		log.Warningf(ctx, "failed to report node usage metrics: status: %s, body: %s", res.Status, b)
		return
	}
	err = r.SQLServer.GetReportedSQLStatsProvider().Reset(ctx)
	if err != nil {
		log.Warningf(ctx, "failed to reset SQL stats: %s", err)
	}
}

// GetLastSuccessfulTelemetryPing will return the timestamp of when we last got
// a ping back from the registration server.
func (r *Reporter) GetLastSuccessfulTelemetryPing() time.Time {
	ts := timeutil.Unix(r.LastSuccessfulTelemetryPing.Load(), 0)
	return ts
}

// CreateReport generates a new diagnostics report containing information about
// the current node or tenant.
func (r *Reporter) CreateReport(
	ctx context.Context, reset telemetry.ResetCounters,
) *diagnosticspb.DiagnosticReport {
	info := diagnosticspb.DiagnosticReport{}
	secret := sql.ClusterSecret.Get(&r.Settings.SV)
	uptime := int64(timeutil.Since(r.StartTime).Seconds())

	// Populate the hardware, OS, binary, and location of the CRDB node or SQL
	// instance.
	r.populateEnvironment(ctx, secret, &info.Env)

	// Always populate SQL info, since even full CRDB running KV will also be
	// running SQL.
	r.populateSQLInfo(uptime, &info.SQL)

	// Do not collect node or store information for tenant reports.
	if r.TenantID == roachpb.SystemTenantID {
		r.populateNodeInfo(ctx, uptime, &info)
	}

	schema, err := r.collectSchemaInfo(ctx)
	if err != nil {
		log.Warningf(ctx, "error collecting schema info for diagnostic report: %+v", err)
		schema = nil
	}
	info.Schema = schema

	info.FeatureUsage = telemetry.GetFeatureCounts(telemetry.Quantized, reset)

	// Read the system.settings table to determine the settings for which we have
	// explicitly set values -- the in-memory SV has the set and default values
	// flattened for quick reads, but we'd rather only report the non-defaults.
	if it, err := r.InternalExec.QueryIteratorEx(
		ctx, "read-setting", nil, /* txn */
		sessiondata.NodeUserSessionDataOverride,
		"SELECT name FROM system.settings",
	); err != nil {
		log.Warningf(ctx, "failed to read settings: %s", err)
	} else {
		info.AlteredSettings = make(map[string]string)
		var ok bool
		for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
			row := it.Cur()
			internalKey := string(tree.MustBeDString(row[0]))
			info.AlteredSettings[internalKey] = settings.RedactedValue(
				settings.InternalKey(internalKey), &r.Settings.SV, r.TenantID == roachpb.SystemTenantID,
			)
		}
		if err != nil {
			// No need to clear AlteredSettings map since we only make best
			// effort to populate it.
			log.Warningf(ctx, "failed to read settings: %s", err)
		}
	}

	if it, err := r.InternalExec.QueryIteratorEx(
		ctx,
		"read-zone-configs",
		nil, /* txn */
		sessiondata.NodeUserSessionDataOverride,
		"SELECT id, config FROM system.zones",
	); err != nil {
		log.Warningf(ctx, "%v", err)
	} else {
		info.ZoneConfigs = make(map[int64]zonepb.ZoneConfig)
		var ok bool
		for ok, err = it.Next(ctx); ok; ok, err = it.Next(ctx) {
			row := it.Cur()
			id := int64(tree.MustBeDInt(row[0]))
			var zone zonepb.ZoneConfig
			if bytes, ok := row[1].(*tree.DBytes); !ok {
				continue
			} else {
				if err := protoutil.Unmarshal([]byte(*bytes), &zone); err != nil {
					log.Warningf(ctx, "unable to parse zone config %d: %v", id, err)
					continue
				}
			}
			var anonymizedZone zonepb.ZoneConfig
			anonymizeZoneConfig(&anonymizedZone, zone, secret)
			info.ZoneConfigs[id] = anonymizedZone
		}
		if err != nil {
			// No need to clear ZoneConfigs map since we only make best effort
			// to populate it.
			log.Warningf(ctx, "%v", err)
		}
	}

	info.SqlStats, err = r.SQLServer.GetScrubbedReportingStats(ctx, 100 /* limit */, false)
	if err != nil {
		if log.V(2 /* level */) {
			log.Warningf(ctx, "unexpected error encountered when getting scrubbed reporting stats: %s", err)
		}
	}

	return &info
}

// populateEnvironment fills fields in the given environment, such as the
// hardware, OS, binary, and location of the CRDB node or SQL instance.
func (r *Reporter) populateEnvironment(
	ctx context.Context, secret string, env *diagnosticspb.Environment,
) {
	env.Build = build.GetInfo()
	env.LicenseType = getLicenseType(ctx, r.Settings)
	populateHardwareInfo(ctx, env)

	// Add in the localities.
	for _, tier := range r.Locality.Tiers {
		env.Locality.Tiers = append(env.Locality.Tiers, roachpb.Tier{
			Key:   sql.HashForReporting(secret, tier.Key),
			Value: sql.HashForReporting(secret, tier.Value),
		})
	}
}

// populateNodeInfo populates the NodeInfo and StoreInfo fields of the
// diagnostics report.
func (r *Reporter) populateNodeInfo(
	ctx context.Context, uptime int64, info *diagnosticspb.DiagnosticReport,
) {
	n := r.Recorder.GenerateNodeStatus(ctx)
	info.Node.NodeID = n.Desc.NodeID
	info.Node.Uptime = uptime

	info.Stores = make([]diagnosticspb.StoreInfo, len(n.StoreStatuses))
	for i, r := range n.StoreStatuses {
		info.Stores[i].NodeID = r.Desc.Node.NodeID
		info.Stores[i].StoreID = r.Desc.StoreID
		info.Stores[i].KeyCount = int64(r.Metrics["keycount"])
		info.Stores[i].Capacity = int64(r.Metrics["capacity"])
		info.Stores[i].Available = int64(r.Metrics["capacity.available"])
		info.Stores[i].Used = int64(r.Metrics["capacity.used"])
		info.Node.KeyCount += info.Stores[i].KeyCount
		info.Stores[i].RangeCount = int64(r.Metrics["replicas"])
		info.Node.RangeCount += info.Stores[i].RangeCount
		bytes := int64(r.Metrics["sysbytes"] + r.Metrics["valbytes"] + r.Metrics["keybytes"] +
			r.Metrics["rangevalbytes"] + r.Metrics["rangekeybytes"])
		info.Stores[i].Bytes = bytes
		info.Node.Bytes += bytes
		info.Stores[i].EncryptionAlgorithm = int64(r.Metrics["rocksdb.encryption.algorithm"])
	}
}

func (r *Reporter) populateSQLInfo(uptime int64, sql *diagnosticspb.SQLInstanceInfo) {
	sql.SQLInstanceID = r.SQLInstanceID()
	sql.Uptime = uptime
}

// collectSchemaInfo is the "old" way of collecting schema information, and it
// redacted all `*string` type fields in the table descriptors but not `string`
// type fields. Check out `schematelemetry` package for a better data source for
// collecting redacted schema information.
func (r *Reporter) collectSchemaInfo(ctx context.Context) ([]descpb.TableDescriptor, error) {
	startKey := keys.MakeSQLCodec(r.TenantID).TablePrefix(keys.DescriptorTableID)
	endKey := startKey.PrefixEnd()
	kvs, err := r.DB.Scan(ctx, startKey, endKey, 0)
	if err != nil {
		return nil, err
	}
	tables := make([]descpb.TableDescriptor, 0, len(kvs))
	redactor := stringRedactor{}
	for _, kv := range kvs {
		b, err := descbuilder.FromSerializedValue(kv.Value)
		if err != nil {
			return nil, errors.Wrapf(err, "%s: unable to unmarshal SQL descriptor", kv.Key)
		}
		if b != nil && b.DescriptorType() == catalog.Table {
			t := b.BuildImmutable().(catalog.TableDescriptor).TableDesc()
			if t.ParentID != keys.SystemDatabaseID {
				if err := reflectwalk.Walk(t, redactor); err != nil {
					panic(err) // stringRedactor never returns a non-nil err
				}
				tables = append(tables, *t)
			}
		}
	}
	return tables, nil
}

// buildReportingURL creates a URL to report diagnostics.
// If an empty updates URL is set (via empty environment variable), returns nil.
func (r *Reporter) buildReportingURL(
	report *diagnosticspb.DiagnosticReport, license *licenseccl.License,
) *url.URL {
	if license == nil {
		license = &licenseccl.License{}
	}

	clusterInfo := ClusterInfo{
		StorageClusterID: r.StorageClusterID(),
		LogicalClusterID: r.LogicalClusterID(),
		TenantID:         r.TenantID,
		IsInsecure:       r.Config.Insecure,
		IsInternal:       sql.ClusterIsInternal(&r.Settings.SV),
		License:          license,
	}

	url := reportingURL
	if r.TestingKnobs != nil && r.TestingKnobs.OverrideReportingURL != nil {
		url = *r.TestingKnobs.OverrideReportingURL
	}
	return addInfoToURL(url, &clusterInfo, &report.Env, report.Node.NodeID, &report.SQL)
}

func getLicenseType(ctx context.Context, settings *cluster.Settings) string {
	licenseType, err := base.LicenseType(settings)
	if err != nil {
		log.Errorf(ctx, "error retrieving license type: %s", err)
		return ""
	}
	return licenseType
}

func anonymizeZoneConfig(dst *zonepb.ZoneConfig, src zonepb.ZoneConfig, secret string) {
	if src.RangeMinBytes != nil {
		dst.RangeMinBytes = proto.Int64(*src.RangeMinBytes)
	}
	if src.RangeMaxBytes != nil {
		dst.RangeMaxBytes = proto.Int64(*src.RangeMaxBytes)
	}
	if src.GC != nil {
		dst.GC = &zonepb.GCPolicy{TTLSeconds: src.GC.TTLSeconds}
	}
	if src.NumReplicas != nil {
		dst.NumReplicas = proto.Int32(*src.NumReplicas)
	}
	dst.Constraints = make([]zonepb.ConstraintsConjunction, len(src.Constraints))
	dst.InheritedConstraints = src.InheritedConstraints
	for i := range src.Constraints {
		dst.Constraints[i].NumReplicas = src.Constraints[i].NumReplicas
		dst.Constraints[i].Constraints = make([]zonepb.Constraint, len(src.Constraints[i].Constraints))
		for j := range src.Constraints[i].Constraints {
			dst.Constraints[i].Constraints[j].Type = src.Constraints[i].Constraints[j].Type
			if key := src.Constraints[i].Constraints[j].Key; key != "" {
				dst.Constraints[i].Constraints[j].Key = sql.HashForReporting(secret, key)
			}
			if val := src.Constraints[i].Constraints[j].Value; val != "" {
				dst.Constraints[i].Constraints[j].Value = sql.HashForReporting(secret, val)
			}
		}
	}
	dst.VoterConstraints = make([]zonepb.ConstraintsConjunction, len(src.VoterConstraints))
	dst.NullVoterConstraintsIsEmpty = src.NullVoterConstraintsIsEmpty
	for i := range src.VoterConstraints {
		dst.VoterConstraints[i].NumReplicas = src.VoterConstraints[i].NumReplicas
		dst.VoterConstraints[i].Constraints = make([]zonepb.Constraint, len(src.VoterConstraints[i].Constraints))
		for j := range src.VoterConstraints[i].Constraints {
			dst.VoterConstraints[i].Constraints[j].Type = src.VoterConstraints[i].Constraints[j].Type
			if key := src.VoterConstraints[i].Constraints[j].Key; key != "" {
				dst.VoterConstraints[i].Constraints[j].Key = sql.HashForReporting(secret, key)
			}
			if val := src.VoterConstraints[i].Constraints[j].Value; val != "" {
				dst.VoterConstraints[i].Constraints[j].Value = sql.HashForReporting(secret, val)
			}
		}
	}
	dst.LeasePreferences = make([]zonepb.LeasePreference, len(src.LeasePreferences))
	dst.InheritedLeasePreferences = src.InheritedLeasePreferences
	for i := range src.LeasePreferences {
		dst.LeasePreferences[i].Constraints = make([]zonepb.Constraint, len(src.LeasePreferences[i].Constraints))
		for j := range src.LeasePreferences[i].Constraints {
			dst.LeasePreferences[i].Constraints[j].Type = src.LeasePreferences[i].Constraints[j].Type
			if key := src.LeasePreferences[i].Constraints[j].Key; key != "" {
				dst.LeasePreferences[i].Constraints[j].Key = sql.HashForReporting(secret, key)
			}
			if val := src.LeasePreferences[i].Constraints[j].Value; val != "" {
				dst.LeasePreferences[i].Constraints[j].Value = sql.HashForReporting(secret, val)
			}
		}
	}
	dst.Subzones = make([]zonepb.Subzone, len(src.Subzones))
	for i := range src.Subzones {
		dst.Subzones[i].IndexID = src.Subzones[i].IndexID
		dst.Subzones[i].PartitionName = sql.HashForReporting(secret, src.Subzones[i].PartitionName)
		anonymizeZoneConfig(&dst.Subzones[i].Config, src.Subzones[i].Config, secret)
	}
}

type stringRedactor struct{}

func (stringRedactor) Primitive(v reflect.Value) error {
	if v.Kind() == reflect.String && v.String() != "" && v.CanSet() {
		v.Set(reflect.ValueOf("_").Convert(v.Type()))
	}
	return nil
}
