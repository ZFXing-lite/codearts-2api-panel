package store

import "testing"

func TestParseCredentialLine(t *testing.T) {
	cases := []struct {
		in     string
		ok     bool
		email  string
		pass   string
		ak, sk string
		opened *bool
	}{
		{"a@b.com----p1----AK1----SK1", true, "a@b.com", "p1", "AK1", "SK1", boolPtr(true)},
		{"a@b.com----p1----开通:是", true, "a@b.com", "p1", "", "", boolPtr(true)},
		{"a@b.com----p1----开通:否", true, "a@b.com", "p1", "", "", boolPtr(false)},
		{"a@b.com---p1---AK1/SK1", true, "a@b.com", "p1", "AK1", "SK1", boolPtr(true)},
		{"a@b.com----p1----AK1/SK1", true, "a@b.com", "p1", "AK1", "SK1", boolPtr(true)},
		{"# comment", false, "", "", "", "", nil},
		{"", false, "", "", "", "", nil},
	}
	for _, c := range cases {
		got, ok := ParseCredentialLine(c.in)
		if ok != c.ok {
			t.Fatalf("%q ok=%v want %v", c.in, ok, c.ok)
		}
		if !ok {
			continue
		}
		if got.Email != c.email || got.Password != c.pass || got.AK != c.ak || got.SK != c.sk {
			t.Fatalf("%q got %+v", c.in, got)
		}
		if c.opened == nil && got.Opened != nil {
			t.Fatalf("%q unexpected opened", c.in)
		}
		if c.opened != nil && (got.Opened == nil || *got.Opened != *c.opened) {
			t.Fatalf("%q opened mismatch", c.in)
		}
	}
}

func boolPtr(v bool) *bool { return &v }
