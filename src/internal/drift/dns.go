// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package drift

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Types are the record types the watcher can look at, in report order.
var Types = []string{"A", "AAAA", "CNAME", "MX", "NS", "TXT", "CAA"}

const typeCAA dnsmessage.Type = 257

var typeOf = map[string]dnsmessage.Type{"A": dnsmessage.TypeA, "AAAA": dnsmessage.TypeAAAA, "CNAME": dnsmessage.TypeCNAME,
	"MX": dnsmessage.TypeMX, "NS": dnsmessage.TypeNS, "TXT": dnsmessage.TypeTXT, "CAA": typeCAA}

// Resolver asks one DNS server, over UDP with a fall back to TCP when the
// answer is cut short. It asks for recursion, so it is pointed at a
// recursive resolver.
type Resolver struct {
	Addr    string // host:port
	Timeout time.Duration
}

// Lookup returns the records of one type at a name, each as text, sorted.
// A name that doesn't exist, or has no records of the type, gives none and
// no error. A timeout, a failed server (SERVFAIL, REFUSED) or a broken
// answer is an error: the records are then unknown, never empty.
func (r Resolver) Lookup(ctx context.Context, name, typ string) ([]string, error) {
	t, ok := typeOf[typ]
	if !ok {
		return nil, fmt.Errorf("unknown record type %s", typ)
	}
	q, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var idb [2]byte
	rand.Read(idb[:])
	id := binary.BigEndian.Uint16(idb[:])
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.EnableCompression()
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: q, Type: t, Class: dnsmessage.ClassINET})
	b.StartAdditionals()
	var opt dnsmessage.ResourceHeader
	opt.SetEDNS0(1232, dnsmessage.RCodeSuccess, false)
	b.OPTResource(opt, dnsmessage.OPTResource{})
	msg, err := b.Finish()
	if err != nil {
		return nil, err
	}
	resp, err := r.exchange(ctx, "udp", msg, id, q, t)
	if err == nil && resp.Header.Truncated {
		resp, err = r.exchange(ctx, "tcp", msg, id, q, t)
	}
	if err != nil {
		return nil, err
	}
	switch resp.Header.RCode {
	case dnsmessage.RCodeSuccess, dnsmessage.RCodeNameError:
	default:
		return nil, fmt.Errorf("the resolver answered %s", strings.TrimPrefix(resp.Header.RCode.String(), "RCode"))
	}
	var out []string
	for _, a := range resp.Answers {
		if a.Header.Type != t {
			continue // the CNAME records on the way to an A record, say
		}
		if v := text(a.Body); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 && !resp.Header.RecursionAvailable && !resp.Header.Authoritative {
		// A referral or a lame answer, from a server that doesn't recurse
		// and doesn't hold the zone: it says nothing about the records.
		return nil, errors.New("the resolver didn't look the name up (no recursion, not authoritative)")
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (r Resolver) exchange(ctx context.Context, network string, msg []byte, id uint16, q dnsmessage.Name, t dnsmessage.Type) (*dnsmessage.Message, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, r.Addr)
	if err != nil {
		return nil, errors.New(short(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if network == "tcp" {
		framed := append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...)
		return r.exchangeTCP(conn, framed, id, q, t)
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, errors.New(short(err))
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, errors.New(short(err))
		}
		var m dnsmessage.Message
		if m.Unpack(buf[:n]) != nil || !matches(&m, id, q, t) {
			continue // a stray or forged packet; wait for the real one
		}
		return &m, nil
	}
}

func (r Resolver) exchangeTCP(conn net.Conn, framed []byte, id uint16, q dnsmessage.Name, t dnsmessage.Type) (*dnsmessage.Message, error) {
	if _, err := conn.Write(framed); err != nil {
		return nil, errors.New(short(err))
	}
	var l [2]byte
	if _, err := io.ReadFull(conn, l[:]); err != nil {
		return nil, errors.New(short(err))
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, errors.New(short(err))
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf); err != nil || !matches(&m, id, q, t) {
		return nil, errors.New("the resolver's answer over TCP is broken")
	}
	return &m, nil
}

func matches(m *dnsmessage.Message, id uint16, q dnsmessage.Name, t dnsmessage.Type) bool {
	return m.Header.Response && m.Header.ID == id && len(m.Questions) == 1 &&
		strings.EqualFold(m.Questions[0].Name.String(), q.String()) && m.Questions[0].Type == t
}

// text writes one record's data the way the report shows it.
func text(body dnsmessage.ResourceBody) string {
	switch b := body.(type) {
	case *dnsmessage.AResource:
		return netip.AddrFrom4(b.A).String()
	case *dnsmessage.AAAAResource:
		return netip.AddrFrom16(b.AAAA).String()
	case *dnsmessage.CNAMEResource:
		return hostText(b.CNAME)
	case *dnsmessage.NSResource:
		return hostText(b.NS)
	case *dnsmessage.MXResource:
		return fmt.Sprintf("%d %s", b.Pref, hostText(b.MX))
	case *dnsmessage.TXTResource:
		return strconv.Quote(strings.Join(b.TXT, ""))
	case *dnsmessage.UnknownResource:
		if b.Type == typeCAA {
			return caaText(b.Data)
		}
	}
	return ""
}

func hostText(n dnsmessage.Name) string {
	return strings.ToLower(strings.TrimSuffix(n.String(), "."))
}

// caaText reads a CAA record (RFC 8659): flags, tag length, tag, value.
func caaText(d []byte) string {
	if len(d) < 2 || len(d) < 2+int(d[1]) {
		return ""
	}
	tag := strings.ToLower(string(d[2 : 2+int(d[1])]))
	return fmt.Sprintf("%d %s %s", d[0], tag, strconv.Quote(string(d[2+int(d[1]):])))
}

// CAA builds a CAA record's data, for tests and the stub resolver.
func CAA(flags byte, tag, value string) []byte {
	return append(append([]byte{flags, byte(len(tag))}, tag...), value...)
}

// short makes a network error short enough for a report line.
func short(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}
