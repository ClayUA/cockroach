// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/cli/exit"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness/livenesspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/apiconstants"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/server/srvtestutils"
	"github.com/cockroachdb/cockroach/pkg/server/status/statuspb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/ui"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/hlc/logger"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSelfBootstrap verifies operation when no bootstrap hosts have
// been specified.
func TestSelfBootstrap(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	if s.RPCContext().StorageClusterID.Get() == uuid.Nil {
		t.Error("cluster ID failed to be set on the RPC context")
	}
}

// TestHealthCheck runs a basic sanity check on the health checker.
func TestHealthCheck(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	cfg := zonepb.DefaultZoneConfig()
	cfg.NumReplicas = proto.Int32(1)
	sCfg := zonepb.DefaultSystemZoneConfig()
	sCfg.NumReplicas = proto.Int32(1)
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Server: &TestingKnobs{
				DefaultZoneConfigOverride:       &cfg,
				DefaultSystemZoneConfigOverride: &sCfg,
			},
		},
	})
	defer s.Stopper().Stop(ctx)
	recorder := s.StorageLayer().MetricsRecorder()

	{
		summary := *recorder.GenerateNodeStatus(ctx)
		result := recorder.CheckHealth(ctx, summary)
		require.Equal(t, 0, len(result.Alerts), "unexpected health alerts: %v", result)
	}

	store, err := s.StorageLayer().GetStores().(*kvserver.Stores).GetStore(1)
	require.NoError(t, err)

	testutils.SucceedsSoon(t, func() error {
		store.Metrics().UnavailableRangeCount.Update(100)
		result := recorder.CheckHealth(ctx, *recorder.GenerateNodeStatus(ctx))
		expAlerts := []statuspb.HealthAlert{
			{StoreID: 1, Category: statuspb.HealthAlert_METRICS, SafeDescription: "ranges.unavailable", Value: 100.0},
		}
		if !reflect.DeepEqual(expAlerts, result.Alerts) {
			return errors.Newf("expected %+v, got %+v", expAlerts, result.Alerts)
		}
		return nil
	})
}

// TestEngineTelemetry tests that the server increments a telemetry counter on
// start that denotes engine type.
func TestEngineTelemetry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	rows, err := db.Query("SELECT * FROM crdb_internal.feature_usage WHERE feature_name LIKE 'storage.engine.%' AND usage_count > 0;")
	defer func() {
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	if count < 1 {
		t.Fatal("expected engine type telemetry counter to be emiitted")
	}
}

// TestServerStartClock tests that a server's clock is not pushed out of thin
// air. This used to happen - the simple act of starting was causing a server's
// clock to be pushed because we were introducing bogus future timestamps into
// our system.
func TestServerStartClock(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Set a high max-offset so that, if the server's clock is pushed by
	// MaxOffset, we don't hide that under the latency of the Start operation
	// which would allow the physical clock to catch up to the pushed one.
	params := base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				MaxOffset: time.Second,
			},
		},
		DisableSQLServer: true,
	}
	s := serverutils.StartServerOnly(t, params)
	defer s.Stopper().Stop(context.Background())

	// Run a command so that we are sure to touch the timestamp cache. This is
	// actually not needed because other commands run during server
	// initialization, but we cannot guarantee that's going to stay that way.
	get := &kvpb.GetRequest{
		RequestHeader: kvpb.RequestHeader{Key: roachpb.Key("a")},
	}
	if _, err := kv.SendWrapped(
		context.Background(), s.DB().NonTransactionalSender(), get,
	); err != nil {
		t.Fatal(err)
	}

	now := s.Clock().Now()
	physicalNow := s.Clock().PhysicalNow()
	serverClockWasPushed := now.WallTime > physicalNow
	if serverClockWasPushed {
		t.Fatalf("time: server %s vs actual %d", now, physicalNow)
	}
}

// TestPlainHTTPServer verifies that we can serve plain http and talk to it.
// This is controlled by -cert=""
func TestPlainHTTPServer(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		// The default context uses embedded certs.
		Insecure: true,
	})
	defer s.Stopper().Stop(context.Background())

	// First, make sure that the testServer's built-in client interface
	// still works in insecure mode.
	var data serverpb.JSONResponse
	testutils.SucceedsSoon(t, func() error {
		return srvtestutils.GetStatusJSONProto(s, "metrics/local", &data)
	})

	// Now make a couple of direct requests using both http and https.
	// They won't succeed because we're not jumping through
	// authentication hoops, but they verify that the server is using
	// the correct protocol.
	url := s.AdminURL().String()
	if !strings.HasPrefix(url, "http://") {
		t.Fatalf("expected insecure admin url to start with http://, but got %s", url)
	}
	if resp, err := httputil.Get(context.Background(), url); err != nil {
		t.Error(err)
	} else {
		func() {
			defer resp.Body.Close()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Error(err)
			}
		}()
	}

	// Attempting to connect to the insecure server with HTTPS doesn't work.
	secureURL := strings.Replace(url, "http://", "https://", 1)
	resp, err := httputil.Get(context.Background(), secureURL)
	if !testutils.IsError(err, "http: server gave HTTP response to HTTPS client") {
		t.Error(err)
	}
	if err == nil {
		resp.Body.Close()
	}
}

func TestSecureHTTPRedirect(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	srv := serverutils.StartServerOnly(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(context.Background())
	ts := srv.ApplicationLayer()

	httpClient, err := ts.GetUnauthenticatedHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	// Avoid automatically following redirects.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	origURL := "http://" + ts.HTTPAddr()
	expURL := url.URL{Scheme: "https", Host: ts.HTTPAddr(), Path: "/"}

	if resp, err := httpClient.Get(origURL); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Errorf("expected status code %d; got %d", http.StatusTemporaryRedirect, resp.StatusCode)
		}
		if redirectURL, err := resp.Location(); err != nil {
			t.Error(err)
		} else if a, e := redirectURL.String(), expURL.String(); a != e {
			t.Errorf("expected location %s; got %s", e, a)
		}
	}

	if resp, err := httpClient.Post(origURL, "text/plain; charset=utf-8", nil); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect {
			t.Errorf("expected status code %d; got %d", http.StatusTemporaryRedirect, resp.StatusCode)
		}
		if redirectURL, err := resp.Location(); err != nil {
			t.Error(err)
		} else if a, e := redirectURL.String(), expURL.String(); a != e {
			t.Errorf("expected location %s; got %s", e, a)
		}
	}
}

// TestAcceptEncoding hits the server while explicitly disabling
// decompression on a custom client's Transport and setting it
// conditionally via the request's Accept-Encoding headers.
func TestAcceptEncoding(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	s := serverutils.StartServerOnly(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())
	client, err := s.GetAdminHTTPClient()
	if err != nil {
		t.Fatal(err)
	}

	var uncompressedSize int64
	var compressedSize int64

	testData := []struct {
		acceptEncoding string
		newReader      func(io.Reader) io.Reader
	}{
		{"",
			func(b io.Reader) io.Reader {
				return b
			},
		},
		{httputil.GzipEncoding,
			func(b io.Reader) io.Reader {
				r, err := gzip.NewReader(b)
				if err != nil {
					t.Fatalf("could not create new gzip reader: %s", err)
				}
				return r
			},
		},
	}
	for _, d := range testData {
		func() {
			req, err := http.NewRequest("GET", s.AdminURL().WithPath(apiconstants.StatusPrefix+"metrics/local").String(), nil)
			if err != nil {
				t.Fatalf("could not create request: %s", err)
			}
			if d.acceptEncoding != "" {
				req.Header.Set(httputil.AcceptEncodingHeader, d.acceptEncoding)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("could not make request to %s: %s", req.URL, err)
			}
			defer resp.Body.Close()
			if ce := resp.Header.Get(httputil.ContentEncodingHeader); ce != d.acceptEncoding {
				t.Fatalf("unexpected content encoding: '%s' != '%s'", ce, d.acceptEncoding)
			}

			// Measure and stash resposne body length for later comparison
			rawBytes, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			rawBodyLength := int64(len(rawBytes))
			if d.acceptEncoding == "" {
				uncompressedSize = rawBodyLength
			} else {
				compressedSize = rawBodyLength
			}

			r := d.newReader(bytes.NewReader(rawBytes))
			var data serverpb.JSONResponse
			if err := jsonpb.Unmarshal(r, &data); err != nil {
				t.Error(err)
			}
		}()
	}

	// Ensure compressed responses are smaller than uncompressed ones when the
	// uncompressed body would be larger than one MTU.
	require.Greater(t, uncompressedSize, int64(1400), "gzip compression testing requires a response body > 1400 bytes (one MTU). Please update the test response.")
	require.Less(t, compressedSize, uncompressedSize, "Compressed response body must be smaller than uncompressed response body")
}

func TestListenerFileCreation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	dir, cleanupFn := testutils.TempDir(t)
	defer cleanupFn()

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		StoreSpecs: []base.StoreSpec{{
			Path: dir,
		}},
	})
	defer s.Stopper().Stop(context.Background())

	files, err := filepath.Glob(filepath.Join(dir, "cockroach.*"))
	if err != nil {
		t.Fatal(err)
	}

	li := listenerInfo{
		listenRPC:    s.RPCAddr(),
		advertiseRPC: s.AdvRPCAddr(),
		listenSQL:    s.SQLAddr(),
		advertiseSQL: s.AdvSQLAddr(),
		listenHTTP:   s.HTTPAddr(),
	}
	expectedFiles := li.Iter()

	for _, file := range files {
		base := filepath.Base(file)
		expVal, ok := expectedFiles[base]
		if !ok {
			t.Fatalf("unexpected file %s", file)
		}
		delete(expectedFiles, base)

		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		addr := string(data)

		if addr != expVal {
			t.Fatalf("expected %s %s to match host %s", base, addr, expVal)
		}
	}

	for f := range expectedFiles {
		t.Errorf("never saw expected file %s", f)
	}
}

func TestClusterIDMismatch(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	engines := make([]storage.Engine, 2)
	for i := range engines {
		e := storage.NewDefaultInMemForTesting()
		defer e.Close()

		sIdent := roachpb.StoreIdent{
			ClusterID: uuid.MakeV4(),
			NodeID:    1,
			StoreID:   roachpb.StoreID(i + 1),
		}
		if err := storage.MVCCPutProto(
			context.Background(), e, keys.StoreIdentKey(), hlc.Timestamp{}, &sIdent, storage.MVCCWriteOptions{},
		); err != nil {
			t.Fatal(err)
		}
		engines[i] = e
	}

	_, err := inspectEngines(
		context.Background(), engines, roachpb.Version{}, roachpb.Version{})
	expected := "conflicting store ClusterIDs"
	if !testutils.IsError(err, expected) {
		t.Fatalf("expected %s error, got %v", expected, err)
	}
}

func TestEnsureInitialWallTimeMonotonicity(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name              string
		prevHLCUpperBound int64
		clockStartTime    int64
		checkPersist      bool
	}{
		{
			name:              "lower upper bound time",
			prevHLCUpperBound: 100,
			clockStartTime:    1000,
			checkPersist:      true,
		},
		{
			name:              "higher upper bound time",
			prevHLCUpperBound: 10000,
			clockStartTime:    1000,
			checkPersist:      true,
		},
		{
			name:              "significantly higher upper bound time",
			prevHLCUpperBound: int64(3 * time.Hour),
			clockStartTime:    int64(1 * time.Hour),
			checkPersist:      true,
		},
		{
			name:              "equal upper bound time",
			prevHLCUpperBound: int64(time.Hour),
			clockStartTime:    int64(time.Hour),
			checkPersist:      true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			a := assert.New(t)

			const maxOffset = 500 * time.Millisecond
			m := timeutil.NewManualTime(timeutil.Unix(0, test.clockStartTime))
			c := hlc.NewClock(m, maxOffset, maxOffset, hlc.PanicLogger)

			sleepUntilFn := func(ctx context.Context, t hlc.Timestamp) error {
				delta := t.GoTime().Sub(c.Now().GoTime())
				if delta > 0 {
					m.Advance(delta)
				}
				return nil
			}

			wallTime1 := c.Now().WallTime
			if test.clockStartTime < test.prevHLCUpperBound {
				a.True(
					wallTime1 < test.prevHLCUpperBound,
					fmt.Sprintf(
						"expected wall time %d < prev upper bound %d",
						wallTime1,
						test.prevHLCUpperBound,
					),
				)
			}

			ensureClockMonotonicity(
				context.Background(),
				c,
				c.PhysicalTime(),
				test.prevHLCUpperBound,
				sleepUntilFn,
			)

			wallTime2 := c.Now().WallTime
			// After ensuring monotonicity, wall time should be greater than
			// persisted upper bound
			a.True(
				wallTime2 > test.prevHLCUpperBound,
				fmt.Sprintf(
					"expected wall time %d > prev upper bound %d",
					wallTime2,
					test.prevHLCUpperBound,
				),
			)
		})
	}
}

func TestPersistHLCUpperBound(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var fatal bool
	defer log.ResetExitFunc()
	log.SetExitFunc(true /* hideStack */, func(r exit.Code) {
		defer log.FlushFiles()
		if r == exit.FatalError() {
			fatal = true
		}
	})

	testCases := []struct {
		name            string
		persistInterval time.Duration
	}{
		{
			name:            "persist default delta",
			persistInterval: 200 * time.Millisecond,
		},
		{
			name:            "persist 100ms delta",
			persistInterval: 50 * time.Millisecond,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			a := assert.New(t)
			m := timeutil.NewManualTime(timeutil.Unix(0, 1))
			c := hlc.NewClock(m, 0 /* maxOffset */, 0 /* toleratedOffset */, logger.CRDBLogger)

			var persistErr error
			var persistedUpperBound int64
			var tickerDur time.Duration
			persistUpperBoundFn := func(i int64) error {
				if persistErr != nil {
					return persistErr
				}
				persistedUpperBound = i
				return nil
			}

			tickerCh := make(chan time.Time)
			tickProcessedCh := make(chan struct{})
			persistHLCUpperBoundIntervalCh := make(chan time.Duration, 1)
			stopCh := make(chan struct{}, 1)
			defer close(stopCh)

			go periodicallyPersistHLCUpperBound(
				c,
				persistHLCUpperBoundIntervalCh,
				persistUpperBoundFn,
				func(d time.Duration) *time.Ticker {
					ticker := time.NewTicker(d)
					ticker.Stop()
					ticker.C = tickerCh
					tickerDur = d
					return ticker
				},
				stopCh,
				func() {
					tickProcessedCh <- struct{}{}
				},
			)

			fatal = false
			// persist an upper bound
			m.Advance(100)
			wallTime3 := c.Now().WallTime
			persistHLCUpperBoundIntervalCh <- test.persistInterval
			<-tickProcessedCh

			a.True(
				test.persistInterval == tickerDur,
				fmt.Sprintf(
					"expected persist interval %d = ticker duration %d",
					test.persistInterval,
					tickerDur,
				),
			)

			// Updating persistInterval should have triggered a persist
			firstPersist := persistedUpperBound
			a.True(
				persistedUpperBound > wallTime3,
				fmt.Sprintf(
					"expected persisted wall time %d > wall time %d",
					persistedUpperBound,
					wallTime3,
				),
			)
			// ensure that in memory value and persisted value are same
			a.Equal(c.WallTimeUpperBound(), persistedUpperBound)

			// Increment clock by 100 and tick the timer.
			// A persist should have happened
			m.Advance(100)
			tickerCh <- timeutil.Now()
			<-tickProcessedCh
			secondPersist := persistedUpperBound
			a.True(
				secondPersist == firstPersist+100,
				fmt.Sprintf(
					"expected persisted wall time %d to be 100 more than earlier persisted value %d",
					secondPersist,
					firstPersist,
				),
			)
			a.Equal(c.WallTimeUpperBound(), persistedUpperBound)
			a.False(fatal)
			fatal = false

			// After disabling persistHLCUpperBound, a value of 0 should be persisted
			persistHLCUpperBoundIntervalCh <- 0
			<-tickProcessedCh
			a.Equal(
				int64(0),
				c.WallTimeUpperBound(),
			)
			a.Equal(int64(0), persistedUpperBound)
			a.Equal(int64(0), c.WallTimeUpperBound())
			a.False(fatal)
			fatal = false

			persistHLCUpperBoundIntervalCh <- test.persistInterval
			<-tickProcessedCh
			m.Advance(100)
			tickerCh <- timeutil.Now()
			<-tickProcessedCh
			// If persisting fails, a fatal error is expected
			persistErr = errors.New("test err")
			fatal = false
			tickerCh <- timeutil.Now()
			<-tickProcessedCh
			a.True(fatal)
		})
	}
}

func TestServeIndexHTML(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const htmlTemplate = `<!DOCTYPE html>
<html>
	<head>
		<title>Cockroach Console</title>
		<meta charset="UTF-8">
		<link href="favicon.ico" rel="shortcut icon">
	</head>
	<body>
		<div id="react-layout"></div>
		<script src="bundle.js" type="text/javascript"></script>
	</body>
</html>
`

	ctx := context.Background()

	linkInFakeUI := func() {
		ui.HaveUI = true
	}
	unlinkFakeUI := func() {
		ui.HaveUI = false
	}
	major, minor := build.BranchReleaseSeries()
	version := fmt.Sprintf("v%d.%d", major, minor)

	t.Run("Insecure mode", func(t *testing.T) {
		srv := serverutils.StartServerOnly(t, base.TestServerArgs{
			Insecure: true,
		})
		defer srv.Stopper().Stop(ctx)
		s := srv.ApplicationLayer()

		client, err := s.GetUnauthenticatedHTTPClient()
		require.NoError(t, err)

		t.Run("short build", func(t *testing.T) {
			resp, err := client.Get(s.AdminURL().String())
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)

			respBytes, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			respString := string(respBytes)
			// Since server test package links ccl, build.GetInfo() says that we
			// think we're using CCL distribution (even though this test file is
			// in `server` and not in `server_test`). However, serverutils
			// doesn't link ccl, so it says it uses OSS distribution. Reconcile
			// this mismatch.
			buildInfo := strings.Replace(build.GetInfo().Short().StripMarkers(), "CockroachDB CCL", "CockroachDB OSS", 1)
			expected := fmt.Sprintf(`<!DOCTYPE html>
<title>CockroachDB</title>
Binary built without web UI.
<hr>
<em>%s</em>`, buildInfo)
			require.Equal(t, expected, respString)
		})

		t.Run("non-short build", func(t *testing.T) {
			linkInFakeUI()
			defer unlinkFakeUI()
			resp, err := client.Get(s.AdminURL().String())
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)

			respBytes, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			respString := string(respBytes)
			require.Equal(t, htmlTemplate, respString)

			resp, err = client.Get(s.AdminURL().WithPath("/uiconfig").String())
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)

			respBytes, err = io.ReadAll(resp.Body)
			require.NoError(t, err)
			expected := fmt.Sprintf(
				`{"Insecure":true,"LoggedInUser":null,"Tag":"%s","Version":"%s","NodeID":"%d","OIDCAutoLogin":false,"OIDCLoginEnabled":false,"OIDCButtonText":"","FeatureFlags":{"can_view_kv_metric_dashboards":true},"OIDCGenerateJWTAuthTokenEnabled":false,"LicenseType":"OSS","SecondsUntilLicenseExpiry":0,"IsManaged":false}`,
				build.GetInfo().Tag,
				version,
				1,
			)
			require.Equal(t, expected, string(respBytes))
		})
	})

	t.Run("Secure mode", func(t *testing.T) {
		linkInFakeUI()
		defer unlinkFakeUI()
		srv := serverutils.StartServerOnly(t, base.TestServerArgs{})
		defer srv.Stopper().Stop(ctx)
		s := srv.ApplicationLayer()

		loggedInClient, err := s.GetAdminHTTPClient()
		require.NoError(t, err)
		loggedOutClient, err := s.GetUnauthenticatedHTTPClient()
		require.NoError(t, err)

		cases := []struct {
			client http.Client
			json   string
		}{
			{
				loggedInClient,
				fmt.Sprintf(
					`{"Insecure":false,"LoggedInUser":"authentic_user","Tag":"%s","Version":"%s","NodeID":"%d","OIDCAutoLogin":false,"OIDCLoginEnabled":false,"OIDCButtonText":"","FeatureFlags":{"can_view_kv_metric_dashboards":true},"OIDCGenerateJWTAuthTokenEnabled":false,"LicenseType":"OSS","SecondsUntilLicenseExpiry":0,"IsManaged":false}`,
					build.GetInfo().Tag,
					version,
					1,
				),
			},
			{
				loggedOutClient,
				fmt.Sprintf(
					`{"Insecure":false,"LoggedInUser":null,"Tag":"%s","Version":"%s","NodeID":"%d","OIDCAutoLogin":false,"OIDCLoginEnabled":false,"OIDCButtonText":"","FeatureFlags":{"can_view_kv_metric_dashboards":true},"OIDCGenerateJWTAuthTokenEnabled":false,"LicenseType":"OSS","SecondsUntilLicenseExpiry":0,"IsManaged":false}`,
					build.GetInfo().Tag,
					version,
					1,
				),
			},
		}

		for i, testCase := range cases {
			t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
				req, err := http.NewRequestWithContext(ctx, "GET", s.AdminURL().String(), nil)
				require.NoError(t, err)

				resp, err := testCase.client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, 200, resp.StatusCode)

				respBytes, err := io.ReadAll(resp.Body)
				require.NoError(t, err)

				respString := string(respBytes)
				require.Equal(t, htmlTemplate, respString)

				req, err = http.NewRequestWithContext(ctx, "GET", s.AdminURL().WithPath("/uiconfig").String(), nil)
				require.NoError(t, err)

				resp, err = testCase.client.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, 200, resp.StatusCode)

				respBytes, err = io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, testCase.json, string(respBytes))
			})
		}
	})

	t.Run("Client-side caching", func(t *testing.T) {
		linkInFakeUI()
		defer unlinkFakeUI()

		// Set up fake asset FS
		mapfs := fstest.MapFS{
			"bundle.js": &fstest.MapFile{
				Data: []byte("console.log('hello world');"),
			},
		}
		fsys, err := mapfs.Sub(".")
		require.NoError(t, err)
		ui.Assets = fsys

		// Clear fake asset FS when we're done
		defer func() {
			ui.Assets = nil
		}()

		srv := serverutils.StartServerOnly(t, base.TestServerArgs{})
		defer srv.Stopper().Stop(ctx)
		s := srv.ApplicationLayer()

		loggedInClient, err := s.GetAdminHTTPClient()
		require.NoError(t, err)
		loggedOutClient, err := s.GetUnauthenticatedHTTPClient()
		require.NoError(t, err)

		cases := []struct {
			desc   string
			client http.Client
		}{
			{
				desc:   "unauthenticated user",
				client: loggedOutClient,
			},
			{
				desc:   "authenticated user",
				client: loggedInClient,
			},
		}

		for _, testCase := range cases {
			t.Run(fmt.Sprintf("bundle caching for %s", testCase.desc), func(t *testing.T) {
				// Request bundle.js without an If-None-Match header first, to simulate the initial load
				uncachedReq, err := http.NewRequestWithContext(ctx, "GET", s.AdminURL().WithPath("/bundle.js").String(), nil)
				require.NoError(t, err)

				uncachedResp, err := testCase.client.Do(uncachedReq)
				require.NoError(t, err)
				defer uncachedResp.Body.Close()
				require.Equal(t, 200, uncachedResp.StatusCode)

				etag := uncachedResp.Header.Get("ETag")
				require.NotEmpty(t, etag, "Server must provide ETag response header with asset responses")

				// Use that ETag header on the next request to simulate a client reload
				cachedReq, err := http.NewRequestWithContext(ctx, "GET", s.AdminURL().WithPath("/bundle.js").String(), nil)
				require.NoError(t, err)
				cachedReq.Header.Add("If-None-Match", etag)

				cachedResp, err := testCase.client.Do(cachedReq)
				require.NoError(t, err)
				defer cachedResp.Body.Close()
				require.Equal(t, 304, cachedResp.StatusCode)

				respBytes, err := io.ReadAll(cachedResp.Body)
				require.NoError(t, err)
				require.Empty(t, respBytes, "Server must provide empty body for cached response")

				etagFromEmptyResp := cachedResp.Header.Get("ETag")
				require.NotEmpty(t, etag, "Server must provide ETag response header with asset responses")

				require.Equal(t, etag, etagFromEmptyResp, "Server must provide consistent ETag response headers")
			})
		}
	})
}

func TestGWRuntimeMarshalProto(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	r, err := http.NewRequest(http.MethodGet, "nope://unused", strings.NewReader(""))
	require.NoError(t, err)
	// Regression test against:
	// https://github.com/cockroachdb/cockroach/issues/49842
	runtime.DefaultHTTPError(
		ctx,
		runtime.NewServeMux(),
		&protoutil.ProtoPb{}, // calls XXX_size
		&httptest.ResponseRecorder{},
		r,
		errors.New("boom"),
	)
}

func TestDecommissionNodeStatus(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc := serverutils.StartCluster(t, 3, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual, // saves time
	})
	defer tc.Stopper().Stop(ctx)
	decomNodeID := tc.Server(2).NodeID()

	// Make sure node status entries have been created.
	for i := 0; i < tc.NumServers(); i++ {
		srv := tc.Server(i)
		entry, err := srv.DB().Get(ctx, keys.NodeStatusKey(srv.NodeID()))
		require.NoError(t, err)
		require.NotNil(t, entry.Value, "node status entry not found for node %d", srv.NodeID())
	}

	// Decommission the node.
	srv := tc.Server(0)
	require.NoError(t, srv.Decommission(
		ctx, livenesspb.MembershipStatus_DECOMMISSIONING, []roachpb.NodeID{decomNodeID}))
	require.NoError(t, srv.Decommission(
		ctx, livenesspb.MembershipStatus_DECOMMISSIONED, []roachpb.NodeID{decomNodeID}))

	// The node status entry should now have been cleaned up.
	entry, err := srv.DB().Get(ctx, keys.NodeStatusKey(decomNodeID))
	require.NoError(t, err)
	require.Nil(t, entry.Value, "found stale node status entry for node %d", decomNodeID)
}

func TestSQLDecommissioned(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc := serverutils.StartCluster(t, 2, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual, // saves time
		ServerArgs: base.TestServerArgs{
			Insecure: true, // to set up a simple SQL client
		},
	})
	defer tc.Stopper().Stop(ctx)

	// Decommission server 1 and wait for it to lose cluster access.
	srv := tc.Server(0)
	decomSrv := tc.Server(1)
	for _, status := range []livenesspb.MembershipStatus{
		livenesspb.MembershipStatus_DECOMMISSIONING, livenesspb.MembershipStatus_DECOMMISSIONED,
	} {
		require.NoError(t, srv.Decommission(ctx, status, []roachpb.NodeID{decomSrv.NodeID()}))
	}

	require.Eventually(t, func() bool {
		_, err := decomSrv.DB().Scan(ctx, keys.MinKey, keys.MaxKey, 0)
		s, ok := status.FromError(errors.UnwrapAll(err))
		return ok && s.Code() == codes.PermissionDenied
	}, 10*time.Second, 100*time.Millisecond, "timed out waiting for server to lose cluster access")

	// Tests SQL access. We're mostly concerned with these operations returning
	// fast rather than hanging indefinitely due to internal retries. We both
	// try the internal executor, to check that the internal structured error is
	// propagated correctly, and also from a client to make sure the error is
	// propagated all the way.
	require.Eventually(t, func() bool {
		sqlExec := decomSrv.InternalExecutor().(*sql.InternalExecutor)

		datums, err := sqlExec.QueryBuffered(ctx, "", nil, "SELECT 1")
		require.NoError(t, err)
		require.Equal(t, []tree.Datums{{tree.NewDInt(1)}}, datums)

		_, err = sqlExec.QueryBuffered(ctx, "", nil, "SELECT * FROM crdb_internal.tables")
		if err == nil {
			return false
		}
		s, ok := status.FromError(errors.UnwrapAll(err))
		require.True(t, ok, "expected gRPC status error, got %T: %s", err, err)
		require.Equal(t, codes.PermissionDenied, s.Code())

		sqlClient := decomSrv.SQLConn(t)

		var result int
		err = sqlClient.QueryRow("SELECT 1").Scan(&result)
		require.NoError(t, err)
		require.Equal(t, 1, result)

		_, err = sqlClient.Query("SELECT * FROM crdb_internal.tables")
		return err != nil
	}, 10*time.Second, 100*time.Millisecond, "timed out waiting for queries to error")
}

func TestAssertEnginesEmpty(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	eng, err := storage.Open(ctx, storage.InMemory(), cluster.MakeClusterSettings())
	require.NoError(t, err)
	defer eng.Close()

	require.NoError(t, assertEnginesEmpty([]storage.Engine{eng}))

	require.NoError(t, storage.MVCCPutProto(ctx, eng, keys.DeprecatedStoreClusterVersionKey(),
		hlc.Timestamp{}, &roachpb.Version{Major: 21, Minor: 1, Internal: 122}, storage.MVCCWriteOptions{}))
	require.NoError(t, assertEnginesEmpty([]storage.Engine{eng}))

	batch := eng.NewBatch()
	key := storage.MVCCKey{
		Key:       []byte{0xde, 0xad, 0xbe, 0xef},
		Timestamp: hlc.Timestamp{WallTime: 100},
	}
	value := storage.MVCCValue{
		Value: roachpb.MakeValueFromString("foo"),
	}
	require.NoError(t, batch.PutMVCC(key, value))
	require.NoError(t, batch.Commit(false))
	require.Error(t, assertEnginesEmpty([]storage.Engine{eng}))
}

// TestAssertExternalStorageInitializedBeforeJobSchedulerStart is a
// bit silly, but the goal is to make sure we don't accidentally move
// things around related to external storage in a way that would break
// the job scheduler.
func TestAssertExternalStorageInitializedBeforeJobSchedulerStart(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	tlServer := &topLevelServer{
		externalStorageBuilder: &externalStorageBuilder{},
	}
	require.Error(t, tlServer.initJobScheduler(context.Background()))
}

func Test_makeFakeNodeStatuses(t *testing.T) {
	defer leaktest.AfterTest(t)()

	n1Desc := roachpb.NodeDescriptor{NodeID: 1}
	n9Desc := roachpb.NodeDescriptor{NodeID: 9}
	tests := []struct {
		name       string
		mapping    map[roachpb.StoreID]roachpb.NodeID
		storesSeen map[string]struct{}

		exp    []statuspb.NodeStatus
		expErr string
	}{
		{
			name:       "store-missing-in-mapping",
			mapping:    map[roachpb.StoreID]roachpb.NodeID{1: 1},
			storesSeen: map[string]struct{}{"1": {}, "2": {}},
			expErr:     `need to map the remaining stores`,
		},
		{
			name:       "store-only-in-mapping",
			mapping:    map[roachpb.StoreID]roachpb.NodeID{1: 1, 3: 9},
			storesSeen: map[string]struct{}{"9": {}},
			expErr:     `s1 supplied in input mapping, but no timeseries found for it`,
		},
		{
			name:       "success",
			mapping:    map[roachpb.StoreID]roachpb.NodeID{1: 1, 3: 9},
			storesSeen: map[string]struct{}{"1": {}, "3": {}},
			exp: []statuspb.NodeStatus{
				{Desc: n1Desc, StoreStatuses: []statuspb.StoreStatus{{Desc: roachpb.StoreDescriptor{
					StoreID: 1,
					Node:    n1Desc,
				}}}},
				{Desc: n9Desc, StoreStatuses: []statuspb.StoreStatus{{Desc: roachpb.StoreDescriptor{
					StoreID: 3,
					Node:    n9Desc,
				}}}},
			},
		},
		{
			name: "success-multi-store",
			// n1 has [s1,s12], n3 has [s3,s6].
			mapping:    map[roachpb.StoreID]roachpb.NodeID{1: 1, 3: 9, 12: 1, 6: 9},
			storesSeen: map[string]struct{}{"1": {}, "3": {}, "6": {}, "12": {}},
			exp: []statuspb.NodeStatus{
				{Desc: n1Desc, StoreStatuses: []statuspb.StoreStatus{
					{Desc: roachpb.StoreDescriptor{StoreID: 1, Node: n1Desc}},
					{Desc: roachpb.StoreDescriptor{StoreID: 12, Node: n1Desc}},
				}},
				{Desc: n9Desc, StoreStatuses: []statuspb.StoreStatus{
					{Desc: roachpb.StoreDescriptor{StoreID: 3, Node: n9Desc}},
					{Desc: roachpb.StoreDescriptor{StoreID: 6, Node: n9Desc}},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := makeFakeNodeStatuses(tt.mapping, nil, nil)
			var err error
			if err = checkFakeStatuses(result, tt.storesSeen); err != nil {
				result = nil
			}
			require.Equal(t, tt.exp, result)
			require.True(t, testutils.IsError(err, tt.expErr), "%+v didn't match expectation %s", err, tt.expErr)
		})
	}
}

// TestStorageBlockLoadConcurrencyLimit verifies that the server correctly
// distributes the limit set by the "storage.block_load.node_max_active"
// cluster setting between the stores.
func TestStorageBlockLoadConcurrencyLimit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	for _, n := range []int{1, 2, 5} {
		t.Run(fmt.Sprintf("%d", n), func(t *testing.T) {
			storeSpecs := make([]base.StoreSpec, n)
			for i := range storeSpecs {
				storeSpecs[i] = base.DefaultTestStoreSpec
			}
			s := serverutils.StartServerOnly(t, base.TestServerArgs{StoreSpecs: storeSpecs})
			defer s.Stopper().Stop(context.Background())

			check := func(expected int64) error {
				return s.GetStores().(*kvserver.Stores).VisitStores(func(s *kvserver.Store) error {
					metrics := s.TODOEngine().GetMetrics()
					if metrics.BlockLoadConcurrencyLimit != expected {
						return fmt.Errorf("expected %d, got %d", expected, metrics.BlockLoadConcurrencyLimit)
					}
					return nil
				})
			}

			require.NoError(t, check((storage.BlockLoadConcurrencyLimit.Default()+int64(n)-1)/int64(n)))

			db := s.SQLConn(t)
			_, err := db.Exec("SET CLUSTER SETTING storage.block_load.node_max_active = 13")
			require.NoError(t, err)
			testutils.SucceedsSoon(t, func() error {
				return check((13 + int64(n) - 1) / int64(n))
			})
		})
	}
}
