package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kel077/pith/internal/api"
)

const (
	throughputTimeout        = 15 * time.Second
	throughputWarmup         = int64(64 << 10)
	throughputControlReserve = int64(1 << 20)
)

type throughputPlan struct {
	Target     string              `json:"target"`
	ServerName string              `json:"serverName"`
	Adapters   [2]throughputBudget `json:"adapters"`
}

type throughputBudget struct {
	Limit    int64 `json:"limitBytes"`
	Download int64 `json:"downloadBytesPerSample"`
	Upload   int64 `json:"uploadBytesPerSample"`
}

// Half the reviewed cap stays unused as headroom for unmetered TCP/IP overhead,
// retransmissions and kernel queues. The meter accounts for TLS socket bytes,
// not a carrier's billing; no finite application meter can bound retransmission.
func (b throughputBudget) reserved() int64 {
	return 3*(throughputSocketLimit(throughputWarmup)+throughputSocketLimit(b.Download)+throughputSocketLimit(b.Upload)) + throughputControlReserve
}

func throughputSocketLimit(payload int64) int64 { return payload + payload/8 + (128 << 10) }

func (s *session) validateThroughput() error {
	p := s.Throughput
	if p == nil {
		return nil
	}
	addr, err := testAddress(p.Target)
	if err != nil || addr.Port() != 443 || addr.String() != p.Target || s.TestProfile != nil {
		return errors.New("throughput requires a canonical public IPv4:443 in host-ipv4 mode")
	}
	name, err := api.ParseTarget(p.ServerName)
	if err != nil || !name.IsDomain() || name.Subdomains || name.String() != p.ServerName {
		return errors.New("throughput-server-name must be a plain lowercase ASCII TLS certificate name")
	}
	for _, b := range p.Adapters {
		if b.Limit < 16<<20 || b.Limit > 1<<40 || b.Download < 64<<10 || b.Download > 256<<20 || b.Upload < 64<<10 || b.Upload > 64<<20 || b.reserved() > b.Limit/2 {
			return errors.New("throughput requires explicit per-adapter caps and bounded samples whose reserved TLS/control bytes fit within half of each cap")
		}
	}
	return nil
}

func (s *session) throughputPlanBytes() ([]byte, error) {
	if s.Throughput == nil {
		return nil, errors.New("prepare and review a throughput plan first")
	}
	if err := s.validateThroughput(); err != nil {
		return nil, err
	}
	plan := struct {
		*throughputPlan
		Interfaces [2]api.InterfaceRef `json:"interfaces"`
		Timeout    time.Duration       `json:"timeoutPerRequest"`
		Warmup     int64               `json:"warmupBytes"`
		Reserved   [2]int64            `json:"reservedBytes"`
		Notes      []string            `json:"notes"`
	}{s.Throughput, s.Interfaces, throughputTimeout, throughputWarmup, [2]int64{s.Throughput.Adapters[0].reserved(), s.Throughput.Adapters[1].reserved()}, []string{
		"One invocation per adapter: direct bound baseline while idle, then one-rule and 1001-rule captured phases; each has one warmup GET, one measured GET and one measured POST.",
		"Freeze HTTP/1.1 GET /__down?bytes=N and POST /__up?bytes=N, literal target and verified TLS name. No redirects, retries, proxies, DNS, parallel streams or replacement targets.",
		"Reserve every phase before the first request in an exclusive durable claim file. Failures, rebaseline and reinstall do not refund or reset the claim.",
		"All test TLS read/write bytes have per-socket caps, including handshake, headers, warmups and errors. Request bodies are generated zeros; response bodies are discarded.",
		"The reservation includes a fixed control allowance; at least half of the reviewed total cap remains unused as network overhead headroom. TCP retransmissions and carrier-billed bytes are not measurable by this harness.",
		"Use identical finite payloads for direct and captured phases. Mbps includes HTTP request/response time, excludes connect/TLS setup; these are short comparisons, not a sustained capacity certification.",
		"Record helper CPU/RSS samples, two-second idle CPU, new connection count and serialized connection-log size. The latter is not on-disk log volume.",
	}}
	b, err := json.MarshalIndent(plan, "", "  ")
	return append(b, '\n'), err
}

func (s *session) writeThroughputPlan() error {
	if s.Throughput == nil {
		return nil
	}
	b, err := s.throughputPlanBytes()
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(s.Dir, "throughput-plan.json"), b); err != nil {
		return err
	}
	s.ThroughputPlanSHA256 = hashBytes(b)
	return s.save()
}

func (s *session) reviewedThroughputPlan() error {
	b, err := s.throughputPlanBytes()
	if err != nil {
		return err
	}
	if hashBytes(b) != s.ThroughputPlanSHA256 {
		return errors.New("throughput endpoint, adapters or budgets changed after preparation")
	}
	p := filepath.Join(s.Dir, "throughput-plan.json")
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("throughput review plan is missing or not a private regular file")
	}
	actual, err := os.ReadFile(p)
	if err != nil || hashBytes(actual) != s.ThroughputPlanSHA256 {
		return errors.New("throughput review plan changed after preparation")
	}
	return nil
}

func throughputAdapter(name string) (int, error) {
	if name == "a" {
		return 0, nil
	}
	if name == "b" {
		return 1, nil
	}
	return 0, errors.New("--throughput must select exactly one reviewed adapter: a or b")
}

// Called under the session lock, before any measurement, control request or
// capture. An exclusive file also prevents reuse after rebaseline/session edits.
func (s *session) claimThroughput(adapter int) error {
	if err := s.reviewedThroughputPlan(); err != nil {
		return err
	}
	if adapter < 0 || adapter > 1 {
		return errors.New("invalid throughput adapter")
	}
	if err := privateTempDirectory(s.Dir); err != nil {
		return err
	}
	file := filepath.Join(s.Dir, fmt.Sprintf("throughput-%c-attempt.json", 'a'+adapter))
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("throughput budget already claimed or cannot be persisted; do not retry without a newly reviewed session and remaining budget")
	}
	claim := struct {
		At       time.Time `json:"at"`
		Plan     string    `json:"planSHA256"`
		Adapter  int       `json:"adapterIndex"`
		Reserved int64     `json:"reservedBytes"`
	}{time.Now(), s.ThroughputPlanSHA256, adapter, s.Throughput.Adapters[adapter].reserved()}
	err = json.NewEncoder(f).Encode(claim)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close()) // Never delete even a partial claim or refund an interrupted attempt.
}
