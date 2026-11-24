package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveUser(t *testing.T) {
	rootfs := t.TempDir()
	etcDir := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	passwd := `# comment line
root:x:0:0:root:/root:/bin/sh
daemon:x:1:1:daemon:/usr/sbin:/sbin/nologin
nginx:x:100:101:nginx:/var/lib/nginx:/sbin/nologin
alice:x:1000:1000:Alice:/home/alice:/bin/ash
nobody:x:65534:65534:nobody:/:/sbin/nologin
malformed:line
baduid:x:notanumber:100::/:
`
	group := `# group comment
root:x:0:root
wheel:x:10:root,alice
www-data:x:82:nginx,alice
nginx:x:101:nginx
alice:x:1000:
docker:x:999:alice,alice
badgid:x:abc:alice
`
	if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etcDir, "group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		spec       string
		wantUID    uint32
		wantGID    uint32
		wantGroups []uint32
		wantHome   string
		wantErr    bool
	}{
		{
			name:       "empty defaults to root",
			spec:       "",
			wantUID:    0,
			wantGID:    0,
			wantGroups: []uint32{10},
			wantHome:   "/root",
		},
		{
			name:       "named user with supplementary groups",
			spec:       "alice",
			wantUID:    1000,
			wantGID:    1000,
			wantGroups: []uint32{10, 82, 999},
			wantHome:   "/home/alice",
		},
		{
			name:       "numeric uid found in passwd inherits gid and supp groups",
			spec:       "100",
			wantUID:    100,
			wantGID:    101,
			wantGroups: []uint32{82},
			wantHome:   "/var/lib/nginx",
		},
		{
			name:       "numeric uid not in passwd defaults to gid 0",
			spec:       "2000",
			wantUID:    2000,
			wantGID:    0,
			wantGroups: nil,
			wantHome:   "/",
		},
		{
			name:       "numeric uid:gid overrides gid and clears supp groups",
			spec:       "1000:82",
			wantUID:    1000,
			wantGID:    82,
			wantGroups: nil,
			wantHome:   "/home/alice",
		},
		{
			name:       "named user:group resolves both and clears supp groups",
			spec:       "alice:www-data",
			wantUID:    1000,
			wantGID:    82,
			wantGroups: nil,
			wantHome:   "/home/alice",
		},
		{
			name:       "named user:numeric gid",
			spec:       "nginx:0",
			wantUID:    100,
			wantGID:    0,
			wantGroups: nil,
			wantHome:   "/var/lib/nginx",
		},
		{
			name:       "numeric uid:named group",
			spec:       "2000:wheel",
			wantUID:    2000,
			wantGID:    10,
			wantGroups: nil,
			wantHome:   "/",
		},
		{
			name:       "empty user with explicit group",
			spec:       ":wheel",
			wantUID:    0,
			wantGID:    10,
			wantGroups: nil,
			wantHome:   "/root",
		},
		{
			name:       "trailing colon treats group as unspecified",
			spec:       "alice:",
			wantUID:    1000,
			wantGID:    1000,
			wantGroups: []uint32{10, 82, 999},
			wantHome:   "/home/alice",
		},
		{
			name:    "unknown user errors",
			spec:    "doesnotexist",
			wantErr: true,
		},
		{
			name:    "unknown group errors",
			spec:    "alice:nogroup",
			wantErr: true,
		},
		{
			name:    "negative uid rejected",
			spec:    "-1",
			wantErr: true,
		},
		{
			name:    "overflow uid rejected",
			spec:    "99999999999999",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUser(rootfs, tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveUser(%q) = %+v, want error", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveUser(%q): %v", tc.spec, err)
			}
			if got.UID != tc.wantUID || got.GID != tc.wantGID || got.Home != tc.wantHome || !slices.Equal(got.Groups, tc.wantGroups) {
				t.Fatalf("resolveUser(%q) = %+v, want uid=%d gid=%d groups=%v home=%q",
					tc.spec, got, tc.wantUID, tc.wantGID, tc.wantGroups, tc.wantHome)
			}
		})
	}
}

func TestResolveUserScratchRootfs(t *testing.T) {
	// Scratch containers have no /etc/passwd or /etc/group.
	scratch := t.TempDir()

	got, err := resolveUser(scratch, "")
	if err != nil || got.UID != 0 || got.GID != 0 || got.Home != "/root" {
		t.Fatalf("scratch empty user = (%+v, %v)", got, err)
	}
	got, err = resolveUser(scratch, "65532:65532")
	if err != nil || got.UID != 65532 || got.GID != 65532 || got.Home != "/" {
		t.Fatalf("scratch numeric user = (%+v, %v)", got, err)
	}
	if _, err := resolveUser(scratch, "nonroot"); err == nil {
		t.Fatalf("scratch named user should fail")
	}
}
