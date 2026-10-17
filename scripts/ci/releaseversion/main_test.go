package main

import "testing"

func TestReleaseSuffix(t *testing.T) {
	for _, test := range []struct {
		name, tag, upstream, want string
	}{
		{"development", "v0.19.0-zlcwwhq.0.1.0", "dev", "zlcwwhq.0.1.0"},
		{"stable", "v0.19.0-zlcwwhq.1.2.3", "", "zlcwwhq.1.2.3"},
		{"prerelease", "v0.19.0-rc1-zlcwwhq.1.2.3", "rc1", "rc1-zlcwwhq.1.2.3"},
		{"upstream tag", "v0.19.0", "", ""},
		{"wrong upstream", "v0.18.1-zlcwwhq.1.2.3", "dev", ""},
		{"missing prefix", "0.19.0-zlcwwhq.1.2.3", "dev", ""},
		{"missing component", "v0.19.0-zlcwwhq.1.2", "dev", ""},
		{"leading zero", "v0.19.0-zlcwwhq.01.2.3", "dev", ""},
		{"trailing text", "v0.19.0-zlcwwhq.1.2.3-rc1", "dev", ""},
		{"wrong prerelease", "v0.19.0-zlcwwhq.1.2.3", "rc1", ""},
		{"shell characters", "v0.19.0-zlcwwhq.1.2.3;echo bad", "dev", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := releaseSuffix(test.tag, "0.19.0", test.upstream)
			if test.want == "" {
				if err == nil {
					t.Fatal("invalid tag accepted")
				}
			} else if err != nil || got != test.want {
				t.Fatalf("got (%q, %v), want %q", got, err, test.want)
			}
		})
	}
}

func TestStampVersion(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		content := "const (" + newline + "\tVersionTag = \"dev\"" + newline + ")" + newline
		want := "const (" + newline + "\tVersionTag = \"zlcwwhq.1.2.3\"" + newline + ")" + newline
		got, err := stampVersion([]byte(content), "zlcwwhq.1.2.3")
		if err != nil || string(got) != want {
			t.Fatalf("newline %q: got (%q, %v), want %q", newline, got, err, want)
		}
	}
	if _, err := stampVersion([]byte("const VersionMajor = 0\n"), "zlcwwhq.1.2.3"); err == nil {
		t.Fatal("missing declaration accepted")
	}
}
