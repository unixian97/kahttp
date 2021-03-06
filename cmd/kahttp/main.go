// Project page; https://github.com/Nordix/kahttp/
// LICENSE; MIT. See the "LICENSE" file in the Project page.
// Copyright (c) 2019, Nordix Foundation

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Nordix/mconnect/pkg/rndip"
	"golang.org/x/net/http2"
	"golang.org/x/time/rate"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var version = "unknown"

const helptext = `
Kahttp attempts to setup many http connections and keep them alive.

Kahttp has 2 modes;

 1. Server - simple server
 2. Client - traffic generator

Options;
`

type config struct {
	isServer  *bool
	hostStats *bool
	disableKA *bool
	http2     *bool
	addr      *string
	nconn     *int
	version   *bool
	timeout   *time.Duration
	monitor   *bool
	psize     *int
	rate      *float64
	stats     *string
	srccidr   *string
	rndip     *rndip.Rndip
	httpsKey  *string
	httpsCert *string
	httpsAddr *string
	logAccess *string
}

const hostHeader = "X-Kahttp-Server-Host"

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), helptext)
		flag.PrintDefaults()
	}

	var cmd config
	cmd.isServer = flag.Bool("server", false, "Act as server")
	cmd.hostStats = flag.Bool("host_stats", false, "Collect server host statistics")
	cmd.disableKA = flag.Bool("disable_ka", false, "Disable keep-alive")
	cmd.http2 = flag.Bool("http2", false, "Use HTTP/2 as client")
	cmd.addr = flag.String("address", "http://127.0.0.1:5080/", "Server address")
	cmd.nconn = flag.Int("nclients", 1, "Number of http clients")
	cmd.version = flag.Bool("version", false, "Print version and quit")
	cmd.timeout = flag.Duration("timeout", 10*time.Second, "Timeout")
	cmd.monitor = flag.Bool("monitor", false, "Monitor")
	psize := 1024
	cmd.psize = &psize
	cmd.rate = flag.Float64("rate", 10.0, "Rate in http-requests/Second")
	cmd.srccidr = flag.String("srccidr", "", "Source CIDR")
	cmd.httpsKey = flag.String(
		"https_key", os.Getenv("KAHTTP_KEY"), "Https secret key file")
	cmd.httpsCert = flag.String(
		"https_cert", os.Getenv("KAHTTP_CERT"), "Https certificate file")
	cmd.httpsAddr = flag.String("https_addr", ":5443", "Https address")
	cmd.logAccess = flag.String(
		"log-access", os.Getenv("LOG_ACCESS"), "Log access (server only)")

	flag.Parse()
	if len(os.Args) < 2 {
		flag.Usage()
		os.Exit(0)
	}

	if *cmd.version {
		fmt.Println(version)
		os.Exit(0)
	}

	if *cmd.isServer {
		os.Exit(cmd.serverMain())
	} else {
		os.Exit(cmd.clientMain())
	}
}

// ----------------------------------------------------------------------
// Client

type ctConn interface {
	Connect(ctx context.Context, address string) error
	Run(ctx context.Context, s *statistics) error
}

// TODO: Use the "connstats" struct in the statistics section
type connData struct {
	id             uint32
	psize          int
	rate           float64
	sent           uint32
	err            error
	started        time.Time
	connected      time.Time
	ended          time.Time
	nFailedConnect uint
	localAddr      net.Addr
}

var cData []connData
var nConn uint32

func (c *config) clientMain() int {

	s := newStats(*c.timeout, *c.rate, *c.nconn, uint32(*c.psize), *c.hostStats)
	rand.Seed(time.Now().UnixNano())

	// The connection array may contain re-connects
	cData = make([]connData, *c.nconn*10)

	deadline := time.Now().Add(*c.timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	if *c.srccidr != "" {
		var err error
		c.rndip, err = rndip.New(*c.srccidr)
		if err != nil {
			log.Fatal("Set source failed:", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(*c.nconn)
	for i := 0; i < *c.nconn; i++ {
		go c.client(ctx, &wg, s)
	}

	if *c.monitor {
		go monitor(s)
	}

	wg.Wait()

	s.reportStats()

	return 0
}

func (c *config) client(ctx context.Context, wg *sync.WaitGroup, s *statistics) {
	defer wg.Done()

	for {

		// Check that we have > 2sec until deadline
		deadline, _ := ctx.Deadline()
		if deadline.Sub(time.Now()) < 2*time.Second {
			return
		}

		// Initiate a new http client
		id := atomic.AddUint32(&nConn, 1) - 1
		if int(id) >= len(cData) {
			log.Fatal("Too many re-connects", id)
		}
		cd := &cData[id]
		cd.id = id
		cd.started = time.Now()
		cd.psize = *c.psize
		cd.rate = *c.rate / float64(*c.nconn)
		if c.rndip != nil {
			sadr := fmt.Sprintf("%s:0", c.rndip.GetIPString())
			if saddr, err := net.ResolveTCPAddr("tcp", sadr); err != nil {
				log.Fatal(err)
			} else {
				cd.localAddr = saddr
			}
		}

		conn := newHTTPConn(cd, c)

		// Connect with re-try and back-off
		backoff := 100 * time.Millisecond
		err := conn.Connect(ctx, *c.addr)
		for err != nil {
			time.Sleep(backoff)
			if backoff < time.Second {
				backoff += 100 * time.Millisecond
			}
			if deadline.Sub(time.Now()) < 2*time.Second {
				cd.ended = s.Started.Add(s.Duration)
				return
			}
			cd.nFailedConnect++
			err = conn.Connect(ctx, *c.addr)
		}
		cd.connected = time.Now()

		cd.err = conn.Run(ctx, s)
		if cd.err == nil {
			// NOTE: The connection *will* stop prematurely if the
			// next packet can't be sent before the dead-line. However
			// the stasistics should show that the connection exists
			// to the test end.
			cd.ended = s.Started.Add(s.Duration)
			return // OK return
		}
		cd.ended = time.Now()

		s.failedConnection(1)
	}

}

func monitor(s *statistics) {
	deadline := s.Started.Add(s.Duration - 1500*time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		var nAct, nConnecting uint
		for _, cd := range cData[:nConn] {
			if cd.err == nil {
				if cd.connected.IsZero() {
					nConnecting++
				} else {
					nAct++
				}
			}
		}
		fmt.Fprintf(
			os.Stderr,
			"Clients act/fail/Dials: %d/%d/%d, Packets send/rec/dropped: %d/%d/%d\n",
			nAct, s.FailedConnections, nDials, s.Sent, s.Received, s.Dropped)
	}
}

func newLimiter(ctx context.Context, r float64, psize int) *rate.Limiter {
	// Allow some burstiness but drain the bucket from start
	// Introduce some ramndomness to spread traffic
	lim := rate.NewLimiter(rate.Limit(r*1024.0), psize*10)
	if lim.WaitN(ctx, rand.Intn(psize)) != nil {
		return nil
	}
	for lim.AllowN(time.Now(), psize) {
	}
	return lim
}

// ----------------------------------------------------------------------
// Http Connection

type httpConn struct {
	cd      *connData
	c       *config
	address string
	client  *http.Client
}

func newHTTPConn(cd *connData, c *config) ctConn {
	return &httpConn{
		cd: cd,
		c:  c,
	}
}

func (c *httpConn) Connect(ctx context.Context, address string) error {
	tr := &http.Transport{
		MaxIdleConns:       1000,
		IdleConnTimeout:    30 * time.Minute,
		DisableCompression: true,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
			LocalAddr: c.cd.localAddr,
			Control:   myControl,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	if *c.c.disableKA {
		tr.DisableKeepAlives = true
	}
	c.client = &http.Client{Transport: tr}
	c.address = address
	if *c.c.http2 {
		if err := http2.ConfigureTransport(tr); err != nil {
			log.Fatal(err)
		}
	}
	return nil
}

// https://stackoverflow.com/questions/52423335/define-tcp-socket-options/52426887
var nDials uint32

func myControl(network, address string, c syscall.RawConn) error {
	atomic.AddUint32(&nDials, 1)
	return nil
}
func (c *httpConn) Run(ctx context.Context, s *statistics) error {

	lim := newLimiter(ctx, c.cd.rate, c.cd.psize)
	if lim == nil {
		return nil
	}

	for {

		if lim.WaitN(ctx, c.cd.psize) != nil {
			break
		}

		resp, err := c.client.Get(c.address)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if *c.c.http2 {
			if !strings.HasPrefix(resp.Proto, "HTTP/2.") {
				log.Fatalf("Incorrect Proto in response; [%s]\n", resp.Proto)
			}
		}

		if s.Hosts != nil {
			// Collect server-host statistics
			if host, ok := resp.Header[hostHeader]; ok && len(host) == 1 {
				sh := host[0]
				// Only kahttp server will do
				if strings.HasPrefix(sh, "Kahttp/") {
					i := strings.Split(sh, "@")
					if len(i) == 2 {
						h := i[1]
						s.mutex.Lock()
						if val, ok := s.Hosts[h]; ok {
							s.Hosts[h] = (val + 1)
						} else {
							s.Hosts[h] = 1
						}
						s.mutex.Unlock()
					}
				}
			}
		}

		s.sent(1)

		for lim.AllowN(time.Now(), c.cd.psize) {
			s.dropped(1)
		}

		if _, err := ioutil.ReadAll(resp.Body); err != nil {
			return err
		}

		s.received(1)
	}

	return nil
}

// ----------------------------------------------------------------------
// Server

type myHandler struct {
	serverHdr string
	logAccess string
	mutex     sync.Mutex
}

func (c *config) serverMain() int {
	var serverEnv = myHandler{
		serverHdr: "Kahttp/" + version,
		logAccess: *c.logAccess,
	}
	if hostName, err := os.Hostname(); err == nil {
		serverEnv.serverHdr += ("@" + hostName)
	}
	if *c.httpsKey != "" && *c.httpsCert != "" {

		// Create a CA certificate pool and add cert.pem to it
		caCert, err := ioutil.ReadFile(*c.httpsCert)
		if err != nil {
			log.Fatal(err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		// Create the TLS Config with the CA pool and enable Client
		// certificate validation
		tlsConfig := &tls.Config{
			ClientCAs: caCertPool,
			ClientAuth: tls.VerifyClientCertIfGiven,
		}
		tlsConfig.BuildNameToCertificate()

		s := &http.Server{
			Addr:			*c.httpsAddr,
			Handler:		myHandler(serverEnv),
			ReadTimeout:	10 * time.Second,
			WriteTimeout:	10 * time.Second,
			IdleTimeout:	10 * time.Second,
			MaxHeaderBytes: 1 << 20,
			TLSConfig: tlsConfig,
		}
		if *c.disableKA {
			s.SetKeepAlivesEnabled(false)
		}

		go func() {
			log.Fatal(s.ListenAndServeTLS(*c.httpsCert, *c.httpsKey))
		}()
	}

	s := &http.Server{
		Addr:           *c.addr,
		Handler:        myHandler(serverEnv),
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		IdleTimeout:    10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	if *c.disableKA {
		s.SetKeepAlivesEnabled(false)
	}
	log.Fatal(s.ListenAndServe())
	return 0
}

func (x myHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(hostHeader, x.serverHdr)
	fmt.Fprintf(w, "Method: %s\n", r.Method)
	fmt.Fprintf(w, "URL: %s\n", r.URL)
	fmt.Fprintf(w, "Proto: %s\n", r.Proto)
	fmt.Fprintf(w, "ContentLength: %d\n", r.ContentLength)
	fmt.Fprintf(w, "TransferEncoding: %v\n", r.TransferEncoding)
	fmt.Fprintf(w, "Host: %s\n", r.Host)
	fmt.Fprintf(w, "RemoteAddr: %s\n", r.RemoteAddr)
	fmt.Fprintf(w, "RequestURI: %s\n", r.RequestURI)
	if r.TLS != nil && r.TLS.PeerCertificates != nil {
		fmt.Fprintf(w, "Tls-nPeerCertificates: %d\n", len(r.TLS.PeerCertificates))
	}
	fmt.Fprintf(w, "%s: %s\n", hostHeader, x)
	for k, v := range r.Header {
		fmt.Fprintf(w, "%s: %s\n", k, v)
	}
	if x.logAccess != "" && strings.HasPrefix(r.RequestURI, x.logAccess) {
		x.mutex.Lock()
		fmt.Printf("%s; %s%s from %s\n", time.Now().Format("2006-01-02 15:04:05"), r.Host, r.RequestURI, r.RemoteAddr)
		x.mutex.Unlock()
	}
}

// ----------------------------------------------------------------------
// Statistics

type statistics struct {
	Started           time.Time
	Duration          time.Duration
	Rate              float64
	Clients           int
	Dials             uint32
	FailedConnections uint32
	Sent              uint32
	Received          uint32
	Dropped           uint32
	FailedConnects    uint
	mutex             sync.Mutex
	Hosts             map[string]int `json:",omitempty"`
}

type sample struct {
	Time     time.Duration
	Sent     uint32
	Received uint32
	Dropped  uint32
}

func newStats(
	duration time.Duration,
	rate float64,
	connections int,
	packetSize uint32,
	hostStats bool) *statistics {

	s := &statistics{
		Started:  time.Now(),
		Duration: duration,
		Rate:     rate,
		Clients:  connections,
	}
	if hostStats {
		s.Hosts = make(map[string]int)
	}
	return s
}

func (s *statistics) sent(n uint32) {
	atomic.AddUint32(&s.Sent, n)
}
func (s *statistics) received(n uint32) {
	atomic.AddUint32(&s.Received, n)
}
func (s *statistics) dropped(n uint32) {
	atomic.AddUint32(&s.Dropped, n)
}
func (s *statistics) failedConnection(n uint32) {
	atomic.AddUint32(&s.FailedConnections, n)
}

func (s *statistics) reportStats() {
	s.Duration = time.Now().Sub(s.Started)
	s.Dials = nDials
	json.NewEncoder(os.Stdout).Encode(s)
}
