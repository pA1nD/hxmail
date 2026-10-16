package outlookmail

import (
	"testing"

	"github.com/pA1nD/hxmail/internal/hxstore/hxbuild"
)

// The fields measured on Outlook 16.114 (README "Changes from m365crawl").

func TestMapFolder114Layout(t *testing.T) {
	f, err := MapFolder(obj(hxbuild.NewMailFolder(hxbuild.MailFolderSpec{Key: 5, Parent: 3, Name: "Fixture Inbox", Type: 0x61, Layout114: true, Lead: 7})))
	if err != nil || f.Name != "Fixture Inbox" || f.Kind != "inbox" || f.Parent != 3 {
		t.Fatalf("%+v %v", f, err)
	}
	junk := hxbuild.MailFolderSpec{Key: 6, Parent: 3, Name: "Fixture Junk", Type: 0x7a, Layout114: true, WellKnown: 7}
	f, err = MapFolder(obj(hxbuild.NewMailFolder(junk)))
	if err != nil || f.Kind != "junk" || f.Generic {
		t.Fatalf("junk: %+v %v", f, err)
	}
	junk.WellKnown = 0xd // Conversation History shares the generic type
	if f, _ := MapFolder(obj(hxbuild.NewMailFolder(junk))); f.Kind != "other" || !f.Generic {
		t.Fatalf("generic: %+v", f)
	}
	// The 16.115 layout carries no known well-known id: a generic folder stays other.
	if f, _ := MapFolder(obj(hxbuild.NewMailFolder(hxbuild.MailFolderSpec{Key: 7, Type: 0x7a}))); f.Kind != "other" {
		t.Fatalf("16.115: %+v", f)
	}
}

func TestMapRecipientRoles(t *testing.T) {
	for role, want := range map[uint32]string{0xcc: "to", 0xcd: "cc", 0xce: "bcc", 0x19d: "reply_to", 0: "", 0xcf: ""} {
		_, r, err := MapRecipient(obj(hxbuild.NewRecipient(hxbuild.RecipientSpec{Key: 1, Parent: 2, Address: "a@example.invalid", Kind: 2, Role: role})))
		if err != nil || r.Role != want || r.KindRaw != 2 {
			t.Errorf("role %#x: %+v %v", role, r, err)
		}
	}
}

func TestMapAttachmentInlineByte(t *testing.T) {
	guid := "0123abcd-4567-89ef-0123-456789ABCDEF"
	for _, c := range []struct {
		name, cid string
		inline    uint8
		want      bool
	}{
		{"image001.png", "image001.png@01DD525D.AA0A49A0", 2, true},
		{"report.pdf", "f_abc123", 0, false}, // Gmail sets a Content-ID on ordinary attachments
		{guid, "", 0, false},                 // a GUID name alone is no sign
		{guid, "", 2, true},
	} {
		a, err := MapAttachment(obj(hxbuild.NewAttachment(hxbuild.AttachmentSpec{Key: 1, MessageKey: 2, Name: c.name, ContentID: c.cid, Inline: c.inline})))
		if err != nil || a.Inline != c.want || a.ContentID != c.cid {
			t.Errorf("%q: %+v %v", c.name, a, err)
		}
	}
	o := hxbuild.NewAttachment(hxbuild.AttachmentSpec{Key: 1, MessageKey: 2, Name: "a.txt", ContentID: "x@y"})
	o.PutU32(580, 4|1<<31) // a present Content-ID whose length runs past the object
	if a, err := MapAttachment(obj(o)); err != nil || a.ContentID != "" || a.BadStrings != 1 {
		t.Fatalf("bad Content-ID: %+v %v", a, err)
	}
}

func TestMapHeaderHasAttachments(t *testing.T) {
	for v, want := range map[uint8]*bool{0: ptr(false), 1: ptr(true), 5: nil} {
		s := headerSpec()
		s.HasAttach = v
		h, err := MapHeader(obj(hxbuild.NewMailHeader(s)))
		if err != nil || (h.HasAttachments == nil) != (want == nil) || want != nil && *h.HasAttachments != *want {
			t.Errorf("byte %d: %v %v", v, h.HasAttachments, err)
		}
	}
}

func ptr(b bool) *bool { return &b }

// Every draft save writes a whole recipient set under a new stamp; only the newest set counts,
// including when a recipient was removed.
func TestCollectKeepsNewestRecipientSet(t *testing.T) {
	r := func(key uint32, stamp uint64, addr string, role uint32) *hxbuild.Object {
		return hxbuild.NewRecipient(hxbuild.RecipientSpec{Key: key, Stamp: stamp, Parent: 21, Address: addr, Role: role})
	}
	objs := append(folderObjs(), hdr(11, 21, fSent, 1, "draft saved three times", 2), det(21, "<a@example.invalid>"),
		r(31, 1, "to@example.invalid", 0xcc),
		r(32, 2, "to@example.invalid", 0xcc), r(33, 2, "cc@example.invalid", 0xcd), r(34, 2, "gone@example.invalid", 0xcd),
		r(35, 3, "to@example.invalid", 0xcc), r(36, 3, "cc@example.invalid", 0xcd), r(37, 3, "bcc@example.invalid", 0xce),
	)
	m := collect(t, storeOf(t, framed(objs...)), Options{}).Messages[0]
	got := map[string]string{}
	for _, rc := range m.Recipients {
		got[rc.Address] = rc.Role
	}
	want := map[string]string{"to@example.invalid": "to", "cc@example.invalid": "cc", "bcc@example.invalid": "bcc"}
	if len(m.Recipients) != 3 || len(got) != 3 || got["to@example.invalid"] != want["to@example.invalid"] ||
		got["cc@example.invalid"] != "cc" || got["bcc@example.invalid"] != "bcc" {
		t.Fatalf("%+v", m.Recipients)
	}
}

// The winner among a clean copy, a resynced copy that repeats its key and a newer resynced copy
// that does not is the same in every store order.
func TestCollectCopyRankingIsOrderIndependent(t *testing.T) {
	clean := framed(hdr(12, 22, fInbox, 10, "clean", 2))
	valid := append(framed(), append([]byte{1, 2, 3}, hdr(12, 22, fInbox, 20, "valid", 2).Encode()...)...)
	brokenObj := hdr(12, 22, fInbox, 30, "broken", 2).Encode()
	brokenObj[4+offKeyAgain] ^= 0xff
	broken := append(framed(), append([]byte{1, 2, 3}, brokenObj...)...)
	base := framed(append(folderObjs(), det(22, "<b@example.invalid>"))...)
	for _, order := range [][3][]byte{
		{clean, valid, broken}, {clean, broken, valid}, {valid, clean, broken},
		{valid, broken, clean}, {broken, clean, valid}, {broken, valid, clean},
	} {
		r := collect(t, storeOf(t, base, order[0], order[1], order[2]), Options{})
		if len(r.Messages) != 1 || r.Messages[0].Subject != "valid" {
			t.Fatalf("%+v", r.Messages)
		}
	}
	// Alone, the broken copy stands in; beside a clean one it loses whatever its stamp.
	if r := collect(t, storeOf(t, base, broken), Options{}); len(r.Messages) != 1 || r.Messages[0].Subject != "broken" {
		t.Fatalf("alone: %+v", r.Messages)
	}
	if r := collect(t, storeOf(t, base, broken, clean), Options{}); r.Messages[0].Subject != "clean" {
		t.Fatalf("beside clean: %+v", r.Messages)
	}
}
