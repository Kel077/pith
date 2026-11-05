package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/engine/capture"
)

type helperUsage struct {
	At       time.Time     `json:"at"`
	PID      int           `json:"pid"`
	Identity uint64        `json:"processUniqueID"`
	CPU      time.Duration `json:"cpuTime"`
	RSS      uint64        `json:"residentBytes"`
}

type throughputPhase struct {
	Name               string             `json:"name"`
	Rules              int                `json:"rules"`
	Before             helperUsage        `json:"helperBefore"`
	After              helperUsage        `json:"helperAfter"`
	CPUPercent         float64            `json:"helperCPUPercentOfOneCore"`
	Samples            []throughputSample `json:"samples"`
	Connections        []api.Connection   `json:"connections,omitempty"`
	SerializedLogBytes int                `json:"serializedConnectionLogBytes"`
	Error              string             `json:"error,omitempty"`
}

type throughputEvidence struct {
	Adapter        string             `json:"adapter"`
	Budget         throughputBudget   `json:"budget"`
	Reserved       int64              `json:"reservedBytes"`
	Phases         []*throughputPhase `json:"phases"`
	IdleBefore     helperUsage        `json:"idleBefore"`
	IdleAfter      helperUsage        `json:"idleAfter"`
	IdleCPUPercent float64            `json:"idleCPUPercentOfOneCore"`
	Notes          []string           `json:"notes"`
}

type throughputDeps struct {
	exchange func(context.Context, *throughputPlan, string, throughputRequest) (throughputSample, error)
	usage    func(context.Context, int) (helperUsage, error)
	pause    func(context.Context, time.Duration) error
	snapshot func(context.Context) (snapshot, error)
}

func liveThroughputChecks(snapshot func(context.Context) (snapshot, error)) throughputDeps {
	return throughputDeps{exchange: func(ctx context.Context, p *throughputPlan, iface string, r throughputRequest) (throughputSample, error) {
		return exchangeThroughput(ctx, p, iface, r, nil, nil)
	}, usage: liveHelperUsage, pause: processPause, snapshot: snapshot}
}

type throughputRun struct {
	s        *session
	adapter  int
	pid      int
	report   *throughputEvidence
	deps     throughputDeps
	baseline snapshot
}

func launchdPID(output string) (int, error) {
	pid := 0
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" {
			n, err := strconv.Atoi(fields[2])
			if err != nil || n <= 1 || pid != 0 {
				return 0, errors.New("ambiguous helper PID in launchd snapshot")
			}
			pid = n
		}
	}
	if pid == 0 {
		return 0, errors.New("running helper PID missing from launchd snapshot")
	}
	return pid, nil
}

func newThroughputRun(s *session, choice string, report *checkReport, deps throughputDeps) (*throughputRun, error) {
	adapter, err := throughputAdapter(choice)
	if err != nil {
		return nil, err
	}
	if deps.exchange == nil || deps.usage == nil || deps.pause == nil || deps.snapshot == nil {
		return nil, errors.New("throughput dependencies unavailable")
	}
	pid, err := launchdPID(report.Before.Launchd)
	if err != nil {
		return nil, err
	}
	if err := s.claimThroughput(adapter); err != nil {
		return nil, err
	}
	evidence := &throughputEvidence{Adapter: choice, Budget: s.Throughput.Adapters[adapter], Reserved: s.Throughput.Adapters[adapter].reserved(), Notes: []string{
		"One reserved attempt; failed or interrupted requests are never retried or refunded.",
		"Single-stream finite HTTP/1.1 samples compare the same payload and adapter. This is not a sustained capacity result or a 500 Mbps certification.",
		"Payload speed excludes TCP/TLS setup, includes HTTP transfer/response time. Socket byte totals include TLS; carrier billing and retransmissions are not measurable here. Half the cap is unused network headroom.",
		"Helper CPU is cumulative user/system time sampled by ps; percentages are relative to one core and may exceed 100. RSS is a point sample, not a peak. Serialized connection JSON is not on-disk log volume.",
	}}
	report.Throughput = evidence
	return &throughputRun{s: s, adapter: adapter, pid: pid, report: evidence, deps: deps, baseline: report.Before}, nil
}

func usageDelta(before, after helperUsage) (float64, error) {
	if before.PID <= 1 || before.PID != after.PID || before.Identity == 0 || before.Identity != after.Identity || after.CPU < before.CPU || !after.At.After(before.At) {
		return 0, errors.New("helper usage identity or clock changed")
	}
	return 100 * float64(after.CPU-before.CPU) / float64(after.At.Sub(before.At)), nil
}

func (r *throughputRun) phase(ctx context.Context, c api.Service, name string, ruleCount int, running *api.Status) (resultErr error) {
	phase := &throughputPhase{Name: name, Rules: ruleCount}
	r.report.Phases = append(r.report.Phases, phase)
	defer func() {
		if resultErr != nil {
			phase.Error = resultErr.Error()
		}
	}()
	before, err := r.deps.snapshot(ctx)
	if err != nil {
		return err
	}
	if !sameCleanupBaseline(r.baseline, throughputExternalView(before, running)) {
		return errors.New("routing or VPN changed before throughput phase")
	}
	selected := r.s.Interfaces[r.adapter].Resolve(before.Interfaces)
	if selected == nil || !selected.Up {
		return errors.New("reviewed throughput adapter is unavailable")
	}
	phase.Before, err = r.deps.usage(ctx, r.pid)
	if err != nil {
		return err
	}
	if len(r.report.Phases) > 1 && phase.Before.Identity != r.report.Phases[0].Before.Identity {
		return errors.New("helper restarted between throughput phases")
	}
	b := r.report.Budget
	requests := []throughputRequest{{Name: "warmup", Bytes: throughputWarmup}, {Name: "download", Bytes: b.Download}, {Name: "upload", Upload: true, Bytes: b.Upload}}
	for _, request := range requests {
		if running == nil {
			st, err := c.Status(ctx)
			if err != nil {
				return err
			}
			if err := idleReviewedStatus(r.s, st); err != nil {
				return err
			}
		} else if err := sameRunningCapture(ctx, c, *running); err != nil {
			return err
		}
		last, err := maxConnectionID(ctx, c)
		if err != nil {
			return err
		}
		iface := ""
		if running == nil {
			iface = selected.Name
		}
		sample, exchangeErr := r.deps.exchange(ctx, r.s.Throughput, iface, request)
		phase.Samples = append(phase.Samples, sample)
		if exchangeErr != nil {
			return exchangeErr
		}
		local, localErr := netip.ParseAddrPort(sample.Local)
		if !sample.Verified || sample.Payload != request.Bytes || sample.Status < 200 || sample.Status >= 300 || sample.Sent <= 0 || sample.Received <= 0 || sample.Sent+sample.Received > throughputSocketLimit(request.Bytes) || sample.Remote != r.s.Throughput.Target || localErr != nil || local.Port() == 0 || local.Addr().IsUnspecified() {
			return errors.New("throughput payload, TLS, response or byte proof incomplete")
		}
		if running != nil {
			conn, err := waitThroughputConnection(ctx, c, last, r.s.Throughput.Target, sample, selected.Name)
			if err != nil {
				return err
			}
			phase.Connections = append(phase.Connections, conn)
		}
	}
	phase.After, err = r.deps.usage(ctx, r.pid)
	if err != nil {
		return err
	}
	phase.CPUPercent, err = usageDelta(phase.Before, phase.After)
	if err != nil {
		return err
	}
	after, err := r.deps.snapshot(ctx)
	if err != nil {
		return err
	}
	if !sameCleanupBaseline(before, after) {
		return errors.New("routing or VPN changed during throughput phase; samples are not comparable")
	}
	if running == nil {
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if err := idleReviewedStatus(r.s, st); err != nil {
			return err
		}
	} else if err := sameRunningCapture(ctx, c, *running); err != nil {
		return err
	}
	if len(phase.Connections) > 0 {
		data, err := json.Marshal(phase.Connections)
		if err != nil {
			return err
		}
		phase.SerializedLogBytes = len(data)
	}
	fmt.Printf("PASS: throughput %s on adapter %c; finite samples recorded.\n", name, 'A'+r.adapter)
	return nil
}

func waitThroughputConnection(parent context.Context, c api.Service, after uint64, target string, sample throughputSample, iface string) (api.Connection, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var found api.Connection
	executable, err := os.Executable()
	if err != nil {
		return found, err
	}
	err = waitUntil(ctx, func() (bool, error) {
		rows, err := c.Connections(ctx, api.ConnectionQuery{Limit: 500})
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.ID <= after || row.Local != sample.Local || row.Remote != target || row.Process.PID != os.Getpid() || row.Protocol != api.TCP {
				continue
			}
			if !canonicalSame(row.Process.Path, executable) || row.RuleID != "throughput-route" || row.Outcome != api.Routed || row.Interface != iface {
				return false, errors.New("throughput tuple used the wrong process, rule or exit")
			}
			out, err := netip.ParseAddrPort(row.OutboundLocal)
			if err != nil || out.Port() == 0 || out.Addr().IsUnspecified() {
				return false, errors.New("throughput outbound tuple unavailable")
			}
			if row.Sent < uint64(sample.Sent) || row.Received < uint64(sample.Received) {
				continue
			}
			found = row
			return true, nil
		}
		return false, nil
	})
	return found, err
}

func throughputRules(p *throughputPlan, iface api.InterfaceRef, count int) []api.Rule {
	rules := make([]api.Rule, 0, count)
	for i := 0; i < count-1; i++ {
		rules = append(rules, api.Rule{ID: fmt.Sprintf("throughput-miss-%d", i), Enabled: true, Targets: []string{fmt.Sprintf("unused-%d.example.com", i)}, Protocol: api.TCP, Action: api.Block})
	}
	addr, _ := netip.ParseAddrPort(p.Target)
	return append(rules, api.Rule{ID: "throughput-route", Enabled: true, Targets: []string{addr.Addr().String()}, Protocol: api.TCP, Action: api.Route, Via: []api.InterfaceRef{iface}})
}

func (r *throughputRun) captured(ctx context.Context, c api.Service, cfg api.Config, running api.Status) error {
	for i, count := range []int{1, 1001} {
		cfg.Scenes[0].Rules = throughputRules(r.s.Throughput, r.s.Interfaces[r.adapter], count)
		if err := c.SaveConfig(ctx, cfg); err != nil {
			return err
		}
		if err := r.phase(ctx, c, []string{"captured-minimal", "captured-many-rules"}[i], count, &running); err != nil {
			return err
		}
	}
	var err error
	r.report.IdleBefore, err = r.deps.usage(ctx, r.pid)
	if err != nil {
		return err
	}
	if err := r.deps.pause(ctx, 2*time.Second); err != nil {
		return err
	}
	if err := sameRunningCapture(ctx, c, running); err != nil {
		return err
	}
	r.report.IdleAfter, err = r.deps.usage(ctx, r.pid)
	if err != nil {
		return err
	}
	r.report.IdleCPUPercent, err = usageDelta(r.report.IdleBefore, r.report.IdleAfter)
	if err != nil {
		return err
	}
	after, err := r.deps.snapshot(ctx)
	if err != nil {
		return err
	}
	if !sameCleanupBaseline(r.baseline, throughputExternalView(after, &running)) {
		return errors.New("routing or VPN changed during idle measurement")
	}
	return nil
}

// Measurement comparison only; this filtered view never authorizes deletion.
// sameRunningCapture separately verifies the live token before every request.
func throughputExternalView(view snapshot, running *api.Status) snapshot {
	if running == nil || running.LastCapture == nil {
		return view
	}
	id := running.LastCapture
	view.Routes = slices.DeleteFunc(slices.Clone(view.Routes), func(route capture.Route) bool { return route.Interface == id.Interface && route.Index == id.Index })
	view.Interfaces = slices.DeleteFunc(slices.Clone(view.Interfaces), func(iface api.Interface) bool { return iface.Name == id.Interface })
	view.InterfaceIndices = maps.Clone(view.InterfaceIndices)
	delete(view.InterfaceIndices, id.Interface)
	view.InterfaceAddresses = maps.Clone(view.InterfaceAddresses)
	delete(view.InterfaceAddresses, id.Interface)
	return view
}
