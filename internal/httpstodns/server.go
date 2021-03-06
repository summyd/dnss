// Package httpstodns implements an HTTPS server which handles DNS requests
// over HTTPS.
package httpstodns

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"blitiri.com.ar/go/dnss/internal/dnsjson"
	"blitiri.com.ar/go/dnss/internal/util"
	"github.com/golang/glog"
	"github.com/miekg/dns"
	"golang.org/x/net/trace"
)

// Server is an HTTPS server that implements DNS over HTTPS, as specified in
// https://developers.google.com/speed/public-dns/docs/dns-over-https#api_specification.
type Server struct {
	Addr     string
	Upstream string
	CertFile string
	KeyFile  string
}

// InsecureForTesting = true will make Server.ListenAndServe will not use TLS.
// This is only useful for integration testing purposes.
var InsecureForTesting = false

// ListenAndServe starts the HTTPS server.
func (s *Server) ListenAndServe() {
	mux := http.NewServeMux()
	mux.HandleFunc("/resolve", s.Resolve)
	srv := http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	glog.Infof("HTTPS listening on %s", s.Addr)
	var err error
	if InsecureForTesting {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(s.CertFile, s.KeyFile)
	}
	glog.Fatalf("HTTPS exiting: %s", err)
}

// Resolve "DNS over HTTPS" requests, and returns responses as specified in
// https://developers.google.com/speed/public-dns/docs/dns-over-https#api_specification.
// It implements an http.HandlerFunc so it can be used with any standard Go
// HTTP server.
func (s *Server) Resolve(w http.ResponseWriter, req *http.Request) {
	tr := trace.New("httpstodns", "/resolve")
	defer tr.Finish()

	tr.LazyPrintf("from:%v", req.RemoteAddr)

	// Construct the DNS request from the http query.
	q, err := parseQuery(req.URL)
	if err != nil {
		util.TraceError(tr, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	r := &dns.Msg{}
	r.CheckingDisabled = q.cd
	r.SetQuestion(dns.Fqdn(q.name), q.rrType)

	if q.clientSubnet != nil {
		o := new(dns.OPT)
		o.Hdr.Name = "."
		o.Hdr.Rrtype = dns.TypeOPT
		e := new(dns.EDNS0_SUBNET)
		e.Code = dns.EDNS0SUBNET
		if ipv4 := q.clientSubnet.IP.To4(); ipv4 != nil {
			e.Family = 1 // IPv4 source address
			e.Address = ipv4
		} else {
			e.Family = 2 // IPv6 source address
			e.Address = q.clientSubnet.IP
		}
		e.SourceScope = 0

		_, maskSize := q.clientSubnet.Mask.Size()
		e.SourceNetmask = uint8(maskSize)

		o.Option = append(o.Option, e)
		r.Extra = append(r.Extra, o)
	}

	util.TraceQuestion(tr, r.Question)

	// Do the DNS request, get the reply.
	fromUp, err := dns.Exchange(r, s.Upstream)
	if err != nil {
		err = util.TraceErrorf(tr, "dns exchange error: %v", err)
		http.Error(w, err.Error(), http.StatusFailedDependency)
		return
	}

	if fromUp == nil {
		err = util.TraceErrorf(tr, "no response from upstream")
		http.Error(w, err.Error(), http.StatusRequestTimeout)
		return
	}

	util.TraceAnswer(tr, fromUp)

	// Convert the reply to json, and write it back.
	jr := &dnsjson.Response{
		Status: fromUp.Rcode,
		TC:     fromUp.Truncated,
		RD:     fromUp.RecursionDesired,
		RA:     fromUp.RecursionAvailable,
		AD:     fromUp.AuthenticatedData,
		CD:     fromUp.CheckingDisabled,
	}

	for _, q := range fromUp.Question {
		rr := dnsjson.RR{
			Name: q.Name,
			Type: q.Qtype,
		}
		jr.Question = append(jr.Question, rr)
	}

	for _, a := range fromUp.Answer {
		hdr := a.Header()
		ja := dnsjson.RR{
			Name: hdr.Name,
			Type: hdr.Rrtype,
			TTL:  hdr.Ttl,
		}

		hs := hdr.String()
		ja.Data = a.String()[len(hs):]
		jr.Answer = append(jr.Answer, ja)
	}

	buf, err := json.Marshal(jr)
	if err != nil {
		err = util.TraceErrorf(tr, "failed to marshal: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(buf)
}

type query struct {
	name   string
	rrType uint16
	cd     bool

	// EDNS client subnet (address+mask).
	clientSubnet *net.IPNet
}

func (q query) String() string {
	return fmt.Sprintf("{%s %d %v %s}", q.name, q.rrType, q.cd, q.clientSubnet)
}

var (
	errEmptyName     = fmt.Errorf("empty name")
	errNameTooLong   = fmt.Errorf("name too long")
	errInvalidSubnet = fmt.Errorf("invalid edns_client_subnet")
	errIntOutOfRange = fmt.Errorf("invalid type (int out of range)")
	errUnknownType   = fmt.Errorf("invalid type (unknown string type)")
	errInvalidCD     = fmt.Errorf("invalid cd value")
)

func parseQuery(u *url.URL) (query, error) {
	q := query{
		name:         "",
		rrType:       1,
		cd:           false,
		clientSubnet: nil,
	}

	// Simplify the values map, as all our parameters are single-value only.
	vs := map[string]string{}
	for k, values := range u.Query() {
		if len(values) > 0 {
			vs[k] = values[0]
		} else {
			vs[k] = ""
		}
	}
	var ok bool
	var err error

	if q.name, ok = vs["name"]; !ok || q.name == "" {
		return q, errEmptyName
	}
	if len(q.name) > 253 {
		return q, errNameTooLong
	}

	if _, ok = vs["type"]; ok {
		q.rrType, err = stringToRRType(vs["type"])
		if err != nil {
			return q, err
		}
	}

	if cd, ok := vs["cd"]; ok {
		q.cd, err = stringToBool(cd)
		if err != nil {
			return q, err
		}
	}

	if clientSubnet, ok := vs["edns_client_subnet"]; ok {
		_, q.clientSubnet, err = net.ParseCIDR(clientSubnet)
		if err != nil {
			return q, errInvalidSubnet
		}
	}

	return q, nil
}

// stringToRRType converts a string into a DNS type constant.
// The string can be a number in the [1, 65535] range, or a canonical type
// string (case-insensitive, such as "A" or "aaaa").
func stringToRRType(s string) (uint16, error) {
	i, err := strconv.ParseInt(s, 10, 16)
	if err == nil {
		if 1 <= i && i <= 65535 {
			return uint16(i), nil
		}
		return 0, errIntOutOfRange
	}

	rrType, ok := dns.StringToType[strings.ToUpper(s)]
	if !ok {
		return 0, errUnknownType
	}
	return rrType, nil
}

func stringToBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "", "1", "true":
		// Note the empty string is intentionally considered true, as long as
		// the parameter is present in the query.
		return true, nil
	case "0", "false":
		return false, nil
	}

	return false, errInvalidCD
}
