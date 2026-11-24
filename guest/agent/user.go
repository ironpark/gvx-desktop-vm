package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// resolvedUser is the numeric identity resolved from a Docker/OCI user spec
// ("" | "uid" | "uid:gid" | "user" | "user:group" | "user:gid" | "uid:group")
// against a container rootfs's /etc/passwd and /etc/group.
type resolvedUser struct {
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
}

type passwdEntry struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

type groupEntry struct {
	Name    string
	GID     uint32
	Members []string
}

// resolveUser parses a Docker-style user specification against <rootfs>/etc/passwd
// and <rootfs>/etc/group.
//
// Semantics match Docker / OCI libcontainer:
//   - Empty spec resolves to uid=0, gid=0 (with /etc/passwd home if present).
//   - Numeric UID without explicit group looks up /etc/passwd by UID for primary
//     GID, home, and supplementary groups; if not found in /etc/passwd, falls
//     back to gid=0 and no supplementary groups.
//   - Named user requires a matching entry in /etc/passwd.
//   - Explicit group (numeric or named) overrides the primary GID and leaves
//     supplementary groups empty (matching `docker run -u user:group`).
func resolveUser(rootfs, spec string) (resolvedUser, error) {
	passwdPath := filepath.Join(rootfs, "etc", "passwd")
	groupPath := filepath.Join(rootfs, "etc", "group")

	users, err := parsePasswdFile(passwdPath)
	if err != nil {
		return resolvedUser{}, err
	}
	groups, err := parseGroupFile(groupPath)
	if err != nil {
		return resolvedUser{}, err
	}
	return resolveUserFromEntries(spec, users, groups)
}

func resolveUserFromEntries(spec string, users []passwdEntry, groups []groupEntry) (resolvedUser, error) {
	spec = strings.TrimSpace(spec)
	userPart, groupPart, hasColon := strings.Cut(spec, ":")
	userPart = strings.TrimSpace(userPart)
	groupPart = strings.TrimSpace(groupPart)
	hasGroup := hasColon && groupPart != ""

	var (
		uid       uint32
		gid       uint32
		home      string
		matched   *passwdEntry
		foundUser bool
	)

	if userPart == "" {
		uid = 0
		gid = 0
		for i := range users {
			if users[i].UID == 0 {
				matched = &users[i]
				gid = users[i].GID
				home = users[i].Home
				foundUser = true
				break
			}
		}
		if home == "" {
			home = "/root"
		}
	} else if parsedUID, ok := parseUint32Strict(userPart); ok {
		uid = parsedUID
		gid = 0
		for i := range users {
			if users[i].UID == parsedUID {
				matched = &users[i]
				gid = users[i].GID
				home = users[i].Home
				foundUser = true
				break
			}
		}
	} else {
		for i := range users {
			if users[i].Name == userPart {
				matched = &users[i]
				uid = users[i].UID
				gid = users[i].GID
				home = users[i].Home
				foundUser = true
				break
			}
		}
		if !foundUser {
			return resolvedUser{}, fmt.Errorf("user %q not found in /etc/passwd", userPart)
		}
	}

	var suppGroups []uint32
	if hasGroup {
		if parsedGID, ok := parseUint32Strict(groupPart); ok {
			gid = parsedGID
		} else {
			foundGroup := false
			for i := range groups {
				if groups[i].Name == groupPart {
					gid = groups[i].GID
					foundGroup = true
					break
				}
			}
			if !foundGroup {
				return resolvedUser{}, fmt.Errorf("group %q not found in /etc/group", groupPart)
			}
		}
	} else if matched != nil {
		suppGroups = collectSupplementaryGroups(matched.Name, gid, groups)
	}

	if home == "" {
		if uid == 0 {
			home = "/root"
		} else {
			home = "/"
		}
	}

	return resolvedUser{
		UID:    uid,
		GID:    gid,
		Groups: suppGroups,
		Home:   home,
	}, nil
}

func collectSupplementaryGroups(username string, primaryGID uint32, groups []groupEntry) []uint32 {
	if username == "" {
		return nil
	}
	var out []uint32
	for _, g := range groups {
		if g.GID == primaryGID {
			continue
		}
		if slices.Contains(g.Members, username) && !slices.Contains(out, g.GID) {
			out = append(out, g.GID)
		}
	}
	return out
}

func parseUint32Strict(s string) (uint32, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func parsePasswdFile(path string) ([]passwdEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return parsePasswd(f), nil
}

func parsePasswd(r io.Reader) []passwdEntry {
	var out []passwdEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, ok1 := parseUint32Strict(strings.TrimSpace(fields[2]))
		gid, ok2 := parseUint32Strict(strings.TrimSpace(fields[3]))
		if !ok1 || !ok2 {
			continue
		}
		home := ""
		if len(fields) >= 6 {
			home = strings.TrimSpace(fields[5])
		}
		out = append(out, passwdEntry{
			Name: strings.TrimSpace(fields[0]),
			UID:  uid,
			GID:  gid,
			Home: home,
		})
	}
	return out
}

func parseGroupFile(path string) ([]groupEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return parseGroup(f), nil
}

func parseGroup(r io.Reader) []groupEntry {
	var out []groupEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		gid, ok := parseUint32Strict(strings.TrimSpace(fields[2]))
		if !ok {
			continue
		}
		var members []string
		if len(fields) >= 4 && strings.TrimSpace(fields[3]) != "" {
			for _, m := range strings.Split(fields[3], ",") {
				m = strings.TrimSpace(m)
				if m != "" {
					members = append(members, m)
				}
			}
		}
		out = append(out, groupEntry{
			Name:    strings.TrimSpace(fields[0]),
			GID:     gid,
			Members: members,
		})
	}
	return out
}
