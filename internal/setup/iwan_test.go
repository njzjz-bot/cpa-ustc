package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
)

type iwanReleaseTransport func(*http.Request) (*http.Response, error)

func (transport iwanReleaseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestInstallIwanUsesLatestStableRelease(t *testing.T) {
	const name = "iwan-client-oidc-linux-x86_64-musl"
	const tag = "v27.3.1"
	const releaseAPI = "https://api.github.com/repos/yyy1mu/ustc-iwan/releases/latest"
	const download = "https://github.com/yyy1mu/ustc-iwan/releases/download/" + tag + "/" + name + ".zip"
	binary := []byte("test executable")
	var bundle bytes.Buffer
	archive := zip.NewWriter(&bundle)
	entry, err := archive.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = entry.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err = archive.Close(); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bundle.Bytes()))

	for _, test := range []struct {
		name, url, digest string
		prerelease        bool
		wantDownloads     int
		wantInstalled     bool
	}{
		{"latest", download, digest, false, 1, true},
		{"prerelease", download, digest, true, 0, false},
		{"wrong source", "https://example.invalid/" + name + ".zip", digest, false, 0, false},
		{"missing digest", download, "", false, 0, false},
		{"wrong digest", download, fmt.Sprintf("sha256:%064d", 0), false, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			downloads := 0
			client := &http.Client{Transport: iwanReleaseTransport(func(request *http.Request) (*http.Response, error) {
				var data []byte
				switch request.URL.String() {
				case releaseAPI:
					data, _ = json.Marshal(map[string]any{
						"tag_name": tag, "prerelease": test.prerelease,
						"assets": []map[string]string{{
							"name": name + ".zip", "browser_download_url": test.url, "digest": test.digest,
						}},
					})
				case download:
					downloads++
					data = bundle.Bytes()
				default:
					t.Fatalf("unexpected request: %s", request.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})}
			path, err := installIwan(context.Background(), t.TempDir(), "linux", name+".zip", client)
			if downloads != test.wantDownloads || (err == nil) != test.wantInstalled {
				t.Fatalf("downloads=%d, path=%q, err=%v", downloads, path, err)
			}
			if test.wantInstalled {
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, binary) {
					t.Fatalf("wrong installed data: %q, %v", data, err)
				}
			}
		})
	}
}
