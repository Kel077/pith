package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Kel077/pith/internal/api"
)

func TestCheckRetainsFailedStartupConnectionsWithoutSubscription(t *testing.T) {
	for _, logFailure := range []bool{false, true} {
		s, service, ops := baselineFixture(t)
		service.startErr = &api.Error{Code: api.ErrNetworkLost, Detail: "synthetic startup failure"}
		service.onStart = func() {
			service.logRows = []api.Connection{{ID: 1, Started: time.Now(), Remote: s.Target, Protocol: api.TCP, Outcome: api.Unreachable, Problem: "example0: network is unreachable"}}
			service.status.RouteDiagnostics = []string{"RTM type=1 flags=0x0 index=90 addrs=[0:family=30/len=28]: synthetic format"}
		}
		if logFailure {
			service.logErr = errors.New("synthetic log unavailable")
		}
		var report *checkReport
		archive := ops.archive
		ops.archive = func(s *session, prefix string, value any) (string, error) {
			if v, ok := value.(checkReport); ok {
				report = &v
			}
			return archive(s, prefix, value)
		}
		// This expected-failure branch starts no subscriber. Only the final log
		// read can retain the outbound failure after the helper has rolled back.
		if err := checkWithOps(context.Background(), s, service, checkOptions{safeFailure: true}, ops); err != nil {
			t.Fatal(err)
		}
		if report == nil || service.stops == 0 || !report.Restored || service.logQuery.Since != report.Started || service.logQuery.Limit != 500 {
			t.Fatal("startup diagnostics did not follow bounded cleanup")
		}
		if !reflect.DeepEqual(report.RouteDiagnostics, service.status.RouteDiagnostics) || len(report.RouteDiagnostics) != 1 {
			t.Fatal("post-Stop report lost startup route metadata", report)
		}
		if logFailure {
			if report.LogError != service.logErr.Error() {
				t.Fatal("log observation failure was hidden")
			}
		} else if !reflect.DeepEqual(report.Connections, service.logRows) || report.LogError != "" {
			t.Fatal("outbound startup error was lost", report)
		}
	}
}
