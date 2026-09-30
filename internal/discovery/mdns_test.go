package discovery

import (
	"reflect"
	"sort"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

type record struct {
	name string
	body dnsmessage.ResourceBody
}

// response builds an mDNS response with answers and additionals, as a Mac's
// mDNSResponder sends them.
func response(t *testing.T, answers, additionals []record) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	add := func(r record) {
		h := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(r.name), Class: dnsmessage.ClassINET, TTL: 120}
		var err error
		switch body := r.body.(type) {
		case *dnsmessage.PTRResource:
			err = b.PTRResource(h, *body)
		case *dnsmessage.SRVResource:
			err = b.SRVResource(h, *body)
		case *dnsmessage.AResource:
			err = b.AResource(h, *body)
		case *dnsmessage.TXTResource:
			err = b.TXTResource(h, *body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	for _, r := range answers {
		add(r)
	}
	if err := b.StartAdditionals(); err != nil {
		t.Fatal(err)
	}
	for _, r := range additionals {
		add(r)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func ptr(service, instance string) record {
	return record{service, &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(instance)}}
}

func srv(instance, target string, port uint16) record {
	return record{instance, &dnsmessage.SRVResource{Target: dnsmessage.MustNewName(target), Port: port}}
}

func a(name string, ip [4]byte) record {
	return record{name, &dnsmessage.AResource{A: ip}}
}

const studio = "Guillermo’s Mac Studio._ssh._tcp.local."

func TestAbsorbResolvesAFullAnswerInOnePacket(t *testing.T) {
	c := newMDNSCache()
	msg := response(t,
		[]record{ptr("_ssh._tcp.local.", studio)},
		[]record{
			srv(studio, "Guillermos-Mac-Studio.local.", 22),
			{studio, &dnsmessage.TXTResource{TXT: []string{""}}},
			a("Guillermos-Mac-Studio.local.", [4]byte{192, 168, 1, 5}),
		})
	if err := c.absorb(msg); err != nil {
		t.Fatal(err)
	}

	got := c.take()
	want := []Host{{
		Name: "Guillermo’s Mac Studio", Host: "Guillermos-Mac-Studio.local",
		Addrs: []string{"192.168.1.5"}, Sources: []Source{SourceBonjour},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("take = %+v, want %+v", got, want)
	}
	if again := c.take(); len(again) != 0 {
		t.Errorf("an unchanged host is reported once, got %+v again", again)
	}
}

func TestAbsorbStitchesAnswersAcrossPackets(t *testing.T) {
	c := newMDNSCache()
	_ = c.absorb(response(t, []record{ptr("_ssh._tcp.local.", studio)}, nil))
	if got := c.take(); len(got) != 0 {
		t.Fatalf("a PTR alone is not a host yet: %+v", got)
	}
	qs := c.followUps()
	if len(qs) != 1 || qs[0].Type != dnsmessage.TypeSRV || qs[0].Name.String() != studio {
		t.Fatalf("followUps = %+v, want one SRV question for the instance", qs)
	}
	if again := c.followUps(); len(again) != 0 {
		t.Errorf("the SRV question is asked once, got %+v", again)
	}

	_ = c.absorb(response(t, []record{srv(studio, "studio.local.", 22)}, nil))
	if got := c.take(); len(got) != 1 || got[0].Host != "studio.local" || len(got[0].Addrs) != 0 {
		t.Fatalf("take after SRV = %+v", got)
	}

	// A later A record re-reports the host with the address it now has.
	_ = c.absorb(response(t, []record{a("studio.local.", [4]byte{10, 0, 0, 9})}, nil))
	if got := c.take(); len(got) != 1 || !reflect.DeepEqual(got[0].Addrs, []string{"10.0.0.9"}) {
		t.Errorf("take after A = %+v", got)
	}
}

func TestAbsorbIgnoresOtherServicesAndOtherPorts(t *testing.T) {
	c := newMDNSCache()
	_ = c.absorb(response(t,
		[]record{
			ptr("_airplay._tcp.local.", "TV._airplay._tcp.local."),
			ptr("_ssh._tcp.local.", "Box._ssh._tcp.local."),
		},
		[]record{
			srv("TV._airplay._tcp.local.", "tv.local.", 7000),
			srv("Box._ssh._tcp.local.", "box.local.", 2222),
		}))
	if got := c.take(); len(got) != 0 {
		t.Errorf("take = %+v; want nothing (not ssh, or not on port 22)", got)
	}
}

func TestAbsorbRejectsQueries(t *testing.T) {
	q, err := query(ptrQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if err := newMDNSCache().absorb(q); err == nil {
		t.Error("our own multicast query echoed back must not be parsed as an answer")
	}
}

func TestFallbacksNameUnresolvedInstancesByTheirLocalHostName(t *testing.T) {
	c := newMDNSCache()
	_ = c.absorb(response(t, []record{
		ptr("_ssh._tcp.local.", studio),
		ptr("_sftp-ssh._tcp.local.", "Resolved._sftp-ssh._tcp.local."),
	}, []record{srv("Resolved._sftp-ssh._tcp.local.", "resolved.local.", 22)}))

	got := c.fallbacks()
	sort.Slice(got, func(i, j int) bool { return got[i].Host < got[j].Host })
	want := []Host{{Name: "Guillermo’s Mac Studio", Host: "guillermos-mac-studio.local", Sources: []Source{SourceBonjour}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallbacks = %+v, want %+v", got, want)
	}
}

func TestSanitizedLocalHostName(t *testing.T) {
	for in, want := range map[string]string{
		"Guillermo’s Mac mini": "guillermos-mac-mini",
		"  build   box ":       "build-box",
		"’’’":                  "’’’",
	} {
		if got := sanitizedLocalHostName(in); got != want {
			t.Errorf("sanitizedLocalHostName(%q) = %q, want %q", in, got, want)
		}
	}
}
