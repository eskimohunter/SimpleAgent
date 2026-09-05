package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseServer serves a fake GitHub release: the releases/latest JSON plus
// a binary asset and its SHA256SUMS. Everything routes through one httptest
// server so the updater's hardcoded repo host is not touched.
type releaseServer struct {
	srv    *httptest.Server
	bin    []byte
	badSum bool // serve a wrong checksum for the binary
	noSums bool // omit the SHA256SUMS asset entirely
	noBin  bool // omit the binary asset entirely
}

func newReleaseServer(t *testing.T, tag string) *releaseServer {
	t.Helper()
	bin := []byte("#!/bin/sh\nfake simpleagent binary\n")
	rs := &releaseServer{bin: bin}
	sums := sha256.Sum256(bin)
	sumsText := fmt.Sprintf("%s  simpleagent-linux-amd64\n", hex.EncodeToString(sums[:]))

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		sumsAsset := fmt.Sprintf(`{"name":"SHA256SUMS","browser_download_url":%q}`, rs.srv.URL+"/SHA256SUMS")
		binAsset := fmt.Sprintf(`{"name":"simpleagent-linux-amd64","browser_download_url":%q}`, rs.srv.URL+"/binary")
		if rs.noBin {
			binAsset = `{"name":"simpleagent-windows-amd64.exe","browser_download_url":"https://example.invalid/b"}`
		}
		if rs.noSums {
			sumsAsset = ""
		}
		assets := binAsset
		if sumsAsset != "" {
			assets += "," + sumsAsset
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[%s]}`, tag, assets)
	})
	mux.HandleFunc("/binary", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(rs.bin)
	})
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		text := sumsText
		if rs.badSum {
			text = strings.Replace(text, hex.EncodeToString(sums[:]), strings.Repeat("0", 64), 1)
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, text)
	})
	rs.srv = httptest.NewServer(mux)
	t.Cleanup(rs.srv.Close)
	return rs
}

// testUpdater points an Updater at the fake server's API path. The repo is
// hardcoded, so the fake must mount /releases/latest on its own host; we
// override the updater fields directly for the test.
func testUpdater(rs *releaseServer) *Updater {
	u := NewUpdater()
	u.repo = rs.srv.URL
	return u
}

func TestLatestRelease(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	up := testUpdater(rs)
	rel, err := up.LatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.2.0" {
		t.Errorf("tag = %q; want v0.2.0", rel.TagName)
	}
	if len(rel.Assets) != 2 {
		t.Errorf("got %d assets; want 2", len(rel.Assets))
	}
}

func TestLatestReleaseNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	up := NewUpdater()
	up.repo = srv.URL
	if _, err := up.LatestRelease(context.Background()); err != ErrNoRelease {
		t.Errorf("err = %v; want ErrNoRelease", err)
	}
}

func TestFindAsset(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	rel, err := testUpdater(rs).LatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ok", func(t *testing.T) {
		bin, sums, err := rel.FindAsset("linux")
		if err != nil {
			t.Fatal(err)
		}
		if bin.Name != "simpleagent-linux-amd64" || sums.Name != "SHA256SUMS" {
			t.Errorf("assets = %q / %q", bin.Name, sums.Name)
		}
	})

	rs.noBin = true
	rel2, _ := testUpdater(rs).LatestRelease(context.Background())
	if _, _, err := rel2.FindAsset("linux"); err == nil ||
		!strings.Contains(err.Error(), "no binary for linux") {
		t.Errorf("missing binary: err = %v", err)
	}

	rs.noBin = false
	rs.noSums = true
	rel3, _ := testUpdater(rs).LatestRelease(context.Background())
	if _, _, err := rel3.FindAsset("linux"); err == nil ||
		!strings.Contains(err.Error(), "no SHA256SUMS asset") {
		t.Errorf("missing checksums: err = %v", err)
	}
}

func TestBinaryName(t *testing.T) {
	if got := binaryName("windows"); got != "simpleagent-windows-amd64.exe" {
		t.Errorf("windows = %q", got)
	}
	if got := binaryName("linux"); got != "simpleagent-linux-amd64" {
		t.Errorf("linux = %q", got)
	}
}

func TestDownload(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	bin, _, _ := rel.FindAsset("linux")

	got, err := up.Download(context.Background(), bin.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(rs.bin) {
		t.Error("downloaded bytes differ from served binary")
	}

	if _, err := up.Download(context.Background(), "ftp://example.invalid/x"); err == nil {
		t.Error("non-http URL was not refused")
	}
}

func TestVerify(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	bin, sums, _ := rel.FindAsset("linux")
	data, _ := up.Download(context.Background(), bin.URL)

	t.Run("match", func(t *testing.T) {
		if err := up.Verify(context.Background(), sums.URL, bin.Name, data); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		rs.badSum = true
		defer func() { rs.badSum = false }()
		if err := up.Verify(context.Background(), sums.URL, bin.Name, data); err == nil {
			t.Fatal("checksum mismatch was not detected")
		}
	})
}

// fakeExe creates a stand-in running binary and returns its path.
func fakeExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "simpleagent")
	if err := os.WriteFile(p, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstall(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	exe := fakeExe(t)

	hexSum, err := up.Install(context.Background(), rel, "linux", exe)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(rs.bin) {
		t.Error("exe was not replaced with the new binary")
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	// The staged file's mode survives the rename, so the executable bit
	// must be restored from the original binary - otherwise the restart
	// and any later manual launch fail with EACCES.
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary lost its execute bit: mode %o", fi.Mode().Perm())
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("installed binary mode = %o; want 755 (preserved from the original)", fi.Mode().Perm())
	}
	sum := sha256.Sum256(rs.bin)
	if hexSum != hex.EncodeToString(sum[:]) {
		t.Errorf("returned checksum %s; want %s", hexSum, hex.EncodeToString(sum[:]))
	}
}

func TestInstallPreservesMode(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	exe := fakeExe(t)
	if err := os.Chmod(exe, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Install(context.Background(), rel, "linux", exe); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Errorf("installed binary mode = %o; want 750 (preserved)", fi.Mode().Perm())
	}
}

func TestInstallRefusesBadChecksum(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	rs.badSum = true
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	exe := fakeExe(t)

	if _, err := up.Install(context.Background(), rel, "linux", exe); err == nil {
		t.Fatal("install with a bad checksum succeeded")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Error("exe was modified despite the checksum mismatch")
	}
}

func TestInstallRefusesWithoutChecksums(t *testing.T) {
	rs := newReleaseServer(t, "v0.2.0")
	rs.noSums = true
	up := testUpdater(rs)
	rel, _ := up.LatestRelease(context.Background())
	exe := fakeExe(t)

	if _, err := up.Install(context.Background(), rel, "linux", exe); err == nil {
		t.Fatal("install without SHA256SUMS succeeded")
	}
}

func TestReplaceExecutable(t *testing.T) {
	exe := fakeExe(t)
	tmp := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(tmp, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(tmp, exe); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "new binary" {
		t.Error("swap did not take effect")
	}
}
