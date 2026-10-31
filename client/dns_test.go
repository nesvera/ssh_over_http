package main

import (
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSAnswer(t *testing.T) {
	addr := netip.MustParseAddr("10.77.0.2")
	tests := []struct {
		qname   string
		qtype   dnsmessage.Type
		rcode   dnsmessage.RCode
		answers int
	}{
		{"dev1.sshtun.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, 1},
		{"DEV1.SSHtun.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, 1},
		{"dev1.sshtun.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, 0},
		{"dev2.sshtun.", dnsmessage.TypeA, dnsmessage.RCodeNameError, 0},
		{"example.com.", dnsmessage.TypeA, dnsmessage.RCodeNameError, 0},
	}
	for _, tt := range tests {
		q := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: 42, RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(tt.qname), Type: tt.qtype, Class: dnsmessage.ClassINET}},
		}
		packed, err := q.Pack()
		if err != nil {
			t.Fatal(err)
		}
		var resp dnsmessage.Message
		if err := resp.Unpack(dnsAnswer(packed, "dev1.sshtun", addr)); err != nil {
			t.Fatalf("%s %v: %v", tt.qname, tt.qtype, err)
		}
		if resp.ID != 42 || !resp.Response || resp.RCode != tt.rcode || len(resp.Answers) != tt.answers {
			t.Errorf("%s %v: got id=%d rcode=%v answers=%d", tt.qname, tt.qtype, resp.ID, resp.RCode, len(resp.Answers))
		}
		if tt.answers == 1 {
			if a, ok := resp.Answers[0].Body.(*dnsmessage.AResource); !ok || netip.AddrFrom4(a.A) != addr {
				t.Errorf("%s: wrong answer %v", tt.qname, resp.Answers[0].Body)
			}
		}
	}
}

func TestDeviceName(t *testing.T) {
	if n, err := deviceName("Dev1", "sshtun."); err != nil || n != "dev1.sshtun" {
		t.Errorf("got %q, %v", n, err)
	}
	for _, bad := range []string{"", "-dev", "dev_1", "a.b"} {
		if _, err := deviceName(bad, "sshtun"); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
