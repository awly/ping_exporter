package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

var (
	port     = flag.String("port", "9101", "port to expose /metrics on")
	target   = flag.String("target", "", "target to ping")
	interval = flag.Duration("interval", 10*time.Second, "ping interval")
	timeout  = flag.Duration("timeout", 10*time.Second, "ping timeout")
)

func main() {
	flag.Parse()

	if *target == "" {
		fmt.Println("--target must be set")
		os.Exit(2)
	}

	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ping",
		Help: "Ping duration.",
	}, []string{"target", "target_ip"})
	prometheus.MustRegister(gauge)

	p := newPinger()
	go p.sendLoop()
	go p.recvLoop(gauge)

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":9101", nil))
}

type pinger struct {
	sync.Mutex
	startTimes map[int]time.Time
	con        *icmp.PacketConn
	remote     *net.IPAddr
	id         int
}

func newPinger() *pinger {
	p := &pinger{
		startTimes: make(map[int]time.Time),
		id:         os.Getpid() & 0xffff,
	}

	con, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		log.Fatal(err)
	}
	p.con = con

	// If the network is unreachable, keep trying.
	for {
		remote, err := net.ResolveIPAddr("ip4", *target)
		if err != nil {
			log.Println(err)
			continue
		}
		p.remote = remote
		break
	}

	return p
}

func (p *pinger) sendLoop() {
	wm := &icmp.Message{
		Type: ipv4.ICMPTypeEcho, Code: 0,
		Body: &icmp.Echo{
			ID:   p.id,
			Seq:  0,
			Data: []byte("ping"),
		},
	}

	for range time.Tick(*interval) {
		wm.Body.(*icmp.Echo).Seq++

		wb, err := wm.Marshal(nil)
		if err != nil {
			// If this happens, there must be a bug. Crash hard.
			log.Fatal(err)
		}

		start := time.Now()
		if _, err := p.con.WriteTo(wb, p.remote); err != nil {
			log.Println(err)
			continue
		}

		p.Lock()

		p.startTimes[wm.Body.(*icmp.Echo).Seq] = start
		// Clean up old entries.
		for k, v := range p.startTimes {
			if time.Since(v) > *timeout {
				log.Println("seq", k, "timed out")
				delete(p.startTimes, k)
			}
		}

		p.Unlock()
	}
}

func (p *pinger) recvLoop(g *prometheus.GaugeVec) {
	buf := make([]byte, 1500)
	for {
		n, peer, err := p.con.ReadFrom(buf)
		if err != nil {
			log.Println(err)
			// Just in case p.con "breaks", don't make this a busy loop hogging
			// a core.
			time.Sleep(time.Second)
			continue
		}

		rm, err := icmp.ParseMessage(1, buf[:n])
		if err != nil {
			log.Println(err)
			continue
		}
		if !matches(rm, p.id) {
			continue
		}

		seq := rm.Body.(*icmp.Echo).Seq
		p.Lock()
		if start, ok := p.startTimes[seq]; ok {
			delete(p.startTimes, seq)
			elapsed := time.Since(start)
			g.With(prometheus.Labels{"target": *target, "target_ip": peer.String()}).
				Set(float64(elapsed / time.Millisecond))
			log.Printf("peer:%v seq:%d %dms", peer, seq, elapsed/time.Millisecond)
		}
		p.Unlock()
	}
}

func matches(rm *icmp.Message, id int) bool {
	if rm.Type != ipv4.ICMPTypeEchoReply {
		return false
	}

	rid := rm.Body.(*icmp.Echo).ID
	if rid != id {
		return false
	}
	return true
}
