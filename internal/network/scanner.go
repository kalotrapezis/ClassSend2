package network

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"classsend/internal/protocol"
)

const (
	ProbePort       = 47821
	ServerPort      = 47820
	ProbeTimeout    = 500 * time.Millisecond
	ScanConcurrency = 50
	ScanInterval    = 30 * time.Second
	ReprobeInterval = 5 * time.Second
)

// Scanner runs on the teacher side — probes IPs looking for student apps
type Scanner struct {
	serverAddr string
	onFound    func(ip string, hs protocol.HandshakePayload)
	// OnMissing is called after each full cycle for each cached student not found.
	// count = consecutive miss count for that MAC.
	OnMissing func(mac, nickname, hostname string, count int)
	devMode   bool

	retryMu  sync.Mutex
	retryIPs map[string]struct{}

	missMu     sync.Mutex
	missCounts map[string]int // MAC → consecutive miss count (in-memory only)
	cycleFound sync.Map       // MACs found in the current scan cycle
}

func NewScanner(serverAddr string, devMode bool, onFound func(string, protocol.HandshakePayload)) *Scanner {
	return &Scanner{
		serverAddr: serverAddr,
		onFound:    onFound,
		devMode:    devMode,
		retryIPs:   make(map[string]struct{}),
		missCounts: make(map[string]int),
	}
}

// AddRetry queues an IP for fast reprobing — call when a student disconnects
func (s *Scanner) AddRetry(ip string) {
	s.retryMu.Lock()
	s.retryIPs[ip] = struct{}{}
	s.retryMu.Unlock()
}

// RunLoop runs indefinitely: fast-path cache → full scan → check misses → wait → repeat
func (s *Scanner) RunLoop(ctx context.Context, cache *MACCache) {
	go s.retryLoop(ctx)

	for {
		s.cycleFound = sync.Map{} // reset per-cycle tracking

		s.fastPath(cache)
		s.scanAll()

		s.checkMisses(cache)

		select {
		case <-ctx.Done():
			return
		case <-time.After(ScanInterval):
		}
	}
}

// checkMisses compares cache entries against what was found this cycle.
// Increments miss counts for absent MACs, resets for found ones, fires OnMissing.
func (s *Scanner) checkMisses(cache *MACCache) {
	if s.OnMissing == nil {
		return
	}
	entries := cache.All()
	s.missMu.Lock()
	defer s.missMu.Unlock()
	for _, e := range entries {
		if _, found := s.cycleFound.Load(e.MAC); found {
			delete(s.missCounts, e.MAC) // back online — reset
		} else {
			s.missCounts[e.MAC]++
			count := s.missCounts[e.MAC]
			// Notify at miss 1 (first absence) and miss 5 (may be down), then every 10
			if count == 1 || count == 5 || count%10 == 0 {
				s.OnMissing(e.MAC, e.Nickname, e.Hostname, count)
			}
		}
	}
}

func (s *Scanner) retryLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(ReprobeInterval):
		}

		s.retryMu.Lock()
		ips := make([]string, 0, len(s.retryIPs))
		for ip := range s.retryIPs {
			ips = append(ips, ip)
		}
		s.retryIPs = make(map[string]struct{})
		s.retryMu.Unlock()

		var wg sync.WaitGroup
		sem := make(chan struct{}, ScanConcurrency)
		for _, ip := range ips {
			ip := ip
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if !s.probe(ip) {
					// Still not back — re-queue for next retry
					s.AddRetry(ip)
				}
			}()
		}
		wg.Wait()
	}
}

func (s *Scanner) fastPath(cache *MACCache) {
	entries := cache.All()
	// Most recently seen first — increases chance of hitting the right IP quickly
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastSeen.After(entries[j].LastSeen)
	})
	s.probeIPs(func(send func(string)) {
		for _, e := range entries {
			// Probe all known IPs for this MAC — handles DHCP changes between sessions
			for _, ip := range e.IPHistory {
				send(ip)
			}
			// LastIP as fallback if history is empty
			if len(e.IPHistory) == 0 && e.LastIP != "" {
				send(e.LastIP)
			}
		}
	})
}


func (s *Scanner) scanAll() {
	nics := GetLocalNICs()
	var wg sync.WaitGroup

	if s.devMode {
		// Dev only: probe own IPs so teacher and student can run on same machine
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, nic := range nics {
				s.probe(nic.IP.String())
			}
			s.probe("127.0.0.1")
		}()
	}

	for _, nic := range nics {
		nic := nic
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.probeIPs(func(send func(string)) {
				for _, ip := range SubnetIPs(nic) {
					send(ip.String())
				}
			})
		}()
	}
	wg.Wait()
}

// probeIPs runs probe() on each IP provided by the generator, with concurrency limit
func (s *Scanner) probeIPs(gen func(func(string))) {
	sem := make(chan struct{}, ScanConcurrency)
	var wg sync.WaitGroup

	gen(func(ip string) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.probe(ip)
		}()
	})

	wg.Wait()
}

// probe connects to one IP's probe port, sends CLASS_HERE, reads handshake preview
// Returns true if a ClassSend student app responded
func (s *Scanner) probe(ip string) bool {
	addr := fmt.Sprintf("%s:%d", ip, ProbePort)
	conn, err := net.DialTimeout("tcp", addr, ProbeTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(ProbeTimeout))

	c := protocol.NewConn(conn)

	msg, err := protocol.Encode(protocol.TypeProbe, protocol.ProbePayload{
		ServerAddr: s.serverAddr,
	})
	if err != nil {
		return false
	}
	if err := c.Send(msg); err != nil {
		return false
	}

	reply, err := c.Recv()
	if err != nil || reply.Type != protocol.TypeHandshake {
		return false
	}

	hs, err := protocol.Decode[protocol.HandshakePayload](reply)
	if err != nil {
		return false
	}

	// Record this MAC as found in the current cycle
	if hs.MAC != "" {
		s.cycleFound.Store(hs.MAC, true)
	}
	if s.onFound != nil {
		s.onFound(ip, hs)
	}
	return true
}
