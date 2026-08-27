package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/rbx-loom/loom-pm/internal/storage"
)

const validManifest = `
[package]
name = "serio"
version = "1.2.0"

[dependencies]
math = "^1.0"
`

type entry struct {
	name     string
	content  string
	kind     byte
	linkname string
}

func file(name, content string) entry {
	return entry{name: name, content: content, kind: tar.TypeReg}
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()

	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	writer := tar.NewWriter(zipper)

	for _, written := range entries {
		header := &tar.Header{
			Name:     written.name,
			Mode:     0o644,
			Typeflag: written.kind,
			Linkname: written.linkname,
			Size:     int64(len(written.content)),
		}

		if written.kind == tar.TypeDir || written.kind == tar.TypeSymlink || written.kind == tar.TypeLink {
			header.Size = 0
		}

		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("writing header %q: %v", written.name, err)
		}

		if header.Size > 0 && written.content != "" {
			if _, err := writer.Write([]byte(written.content)); err != nil {
				t.Fatalf("writing %q: %v", written.name, err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("closing the tar: %v", err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatalf("closing the gzip: %v", err)
	}

	return compressed.Bytes()
}

func valid(t *testing.T) []byte {
	t.Helper()

	return archive(t,
		file("loom-config.toml", validManifest),
		file("src/init.loom", "export fn main() {}"),
		file("src/inner/helper.loom", "fn helper() {}"),
		file("README.md", "# serio"),
	)
}

func rejects(t *testing.T, content []byte, because string) {
	t.Helper()

	payload, err := Read(content, DefaultLimits())
	if err == nil {
		t.Fatalf("Read accepted %d bytes holding %v, want it rejected because it %s", len(content), payload.Files, because)
	}
}

func TestReadAcceptsAPackage(t *testing.T) {
	content := valid(t)

	payload, err := Read(content, DefaultLimits())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if payload.Manifest.Package == nil {
		t.Fatal("no [package] was read")
	}

	if got := payload.Manifest.Package.Name.String(); got != "serio" {
		t.Errorf("name = %q, want serio", got)
	}

	if got := payload.Manifest.Package.Version.String(); got != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", got)
	}

	if len(payload.Manifest.Dependencies) != 1 {
		t.Errorf("read %d dependencies, want 1", len(payload.Manifest.Dependencies))
	}

	if payload.Digest != storage.DigestOf(content) {
		t.Error("the digest is not the digest of the uploaded bytes")
	}

	want := []string{"README.md", "loom-config.toml", "src/init.loom", "src/inner/helper.loom"}
	if strings.Join(payload.Files, ",") != strings.Join(want, ",") {
		t.Errorf("files = %v, want %v sorted", payload.Files, want)
	}
}

func TestReadIgnoresDirectoryEntries(t *testing.T) {
	content := archive(t,
		entry{name: "src/", kind: tar.TypeDir},
		file("loom-config.toml", validManifest),
		file("src/init.loom", "export fn main() {}"),
	)

	payload, err := Read(content, DefaultLimits())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	for _, name := range payload.Files {
		if strings.HasSuffix(name, "/") {
			t.Errorf("files include the directory %q", name)
		}
	}

	if len(payload.Files) != 2 {
		t.Errorf("files = %v, want only the two real files", payload.Files)
	}
}

func TestReadRequiresAManifestAtTheRoot(t *testing.T) {
	rejects(t, archive(t, file("src/init.loom", "fn main() {}")), "has no manifest")

	rejects(t, archive(t,
		file("serio-1.2.0/loom-config.toml", validManifest),
		file("serio-1.2.0/src/init.loom", "fn main() {}"),
	), "wraps the manifest in a directory")
}

func TestReadRequiresAPackageTable(t *testing.T) {
	rejects(t, archive(t,
		file("loom-config.toml", "project_type = \"game\"\n"),
		file("src/init.loom", "fn main() {}"),
	), "has no identity to publish under")
}

// Mirrors PackagePublisher.Prepare, which refuses to publish a project whose source
// directory holds no .loom files.
func TestReadRequiresALoomFile(t *testing.T) {
	rejects(t, archive(t,
		file("loom-config.toml", validManifest),
		file("README.md", "# serio"),
	), "holds no .loom files")
}

func TestReadRejectsUnsafePaths(t *testing.T) {
	cases := []struct{ name, path string }{
		{"parent traversal", "../evil.loom"},
		{"nested traversal", "src/../../evil.loom"},
		{"absolute", "/etc/passwd"},
		{"backslash", `src\evil.loom`},
		{"current directory", "."},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rejects(t, archive(t,
				file("loom-config.toml", validManifest),
				file("src/init.loom", "fn main() {}"),
				file(testCase.path, "malicious"),
			), "names an unsafe path")
		})
	}
}

// A package is source, so anything that is not a regular file is something the registry
// has no reason to carry and every reason not to hand to an extractor.
func TestReadRejectsIrregularEntries(t *testing.T) {
	cases := []struct {
		name string
		kind byte
		link string
	}{
		{"symlink", tar.TypeSymlink, "/etc/passwd"},
		{"hardlink", tar.TypeLink, "loom-config.toml"},
		{"fifo", tar.TypeFifo, ""},
		{"character device", tar.TypeChar, ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rejects(t, archive(t,
				file("loom-config.toml", validManifest),
				file("src/init.loom", "fn main() {}"),
				entry{name: "src/sneaky", kind: testCase.kind, linkname: testCase.link},
			), "is not a regular file")
		})
	}
}

// Two entries under one name make what was published ambiguous, and let an extractor and
// a validator disagree about which one is the package.
func TestReadRejectsDuplicatePaths(t *testing.T) {
	rejects(t, archive(t,
		file("loom-config.toml", validManifest),
		file("src/init.loom", "fn main() {}"),
		file("src/init.loom", "fn main() { evil() }"),
	), "names one path twice")
}

func TestReadRejectsAnOversizedUpload(t *testing.T) {
	limits := DefaultLimits()
	limits.CompressedBytes = 32

	if _, err := Read(valid(t), limits); err == nil {
		t.Error("Read accepted an upload past the compressed limit")
	}
}

func TestReadRejectsTooManyFiles(t *testing.T) {
	limits := DefaultLimits()
	limits.Files = 2

	if _, err := Read(valid(t), limits); err == nil {
		t.Error("Read accepted an archive past the file limit")
	}
}

func TestReadRejectsAnOversizedFile(t *testing.T) {
	limits := DefaultLimits()
	limits.FileBytes = 8

	if _, err := Read(valid(t), limits); err == nil {
		t.Error("Read accepted a file past the per-file limit")
	}
}

// Highly compressible content is small on the wire and large on disk, so the limit that
// matters is on what an archive expands to rather than on what was uploaded.
func TestReadRejectsAGzipBomb(t *testing.T) {
	limits := DefaultLimits()
	limits.DecompressedBytes = 1024

	bomb := archive(t,
		file("loom-config.toml", validManifest),
		file("src/init.loom", "fn main() {}"),
		file("src/bomb.loom", strings.Repeat("\x00", 200_000)),
	)

	if int64(len(bomb)) > limits.CompressedBytes {
		t.Fatalf("the bomb is %d bytes compressed, which the upload limit would catch first", len(bomb))
	}

	if _, err := Read(bomb, limits); err == nil {
		t.Error("Read accepted an archive expanding past the limit")
	}
}

// safePath is exercised directly for names archive/tar refuses to even write, which a
// hand-crafted archive is under no obligation to avoid.
func TestSafePathRejects(t *testing.T) {
	cases := []struct{ name, path string }{
		{"empty", ""},
		{"null byte", "src/evil\x00.loom"},
		{"parent traversal", "../evil.loom"},
		{"absolute", "/etc/passwd"},
		{"backslash", `src\evil.loom`},
		{"current directory", "."},
		{"bare parent", ".."},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if cleaned, err := safePath(testCase.path); err == nil {
				t.Errorf("safePath(%q) = %q, want an error", testCase.path, cleaned)
			}
		})
	}
}

func TestSafePathNormalises(t *testing.T) {
	cases := []struct{ path, want string }{
		{"loom-config.toml", "loom-config.toml"},
		{"./loom-config.toml", "loom-config.toml"},
		{"src/init.loom", "src/init.loom"},
		{"src/./inner/helper.loom", "src/inner/helper.loom"},
		{"src/inner/../init.loom", "src/init.loom"},
	}

	for _, testCase := range cases {
		t.Run(testCase.path, func(t *testing.T) {
			cleaned, err := safePath(testCase.path)
			if err != nil {
				t.Fatalf("safePath(%q): %v", testCase.path, err)
			}

			if cleaned != testCase.want {
				t.Errorf("safePath(%q) = %q, want %q", testCase.path, cleaned, testCase.want)
			}
		})
	}
}

func TestReadRejectsMalformedArchives(t *testing.T) {
	rejects(t, []byte("not gzip at all"), "is not gzip")

	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	zipper.Write([]byte("gzip, but not a tar"))
	zipper.Close()

	rejects(t, compressed.Bytes(), "is not a tar")
	rejects(t, nil, "is empty")
}

// The manifest is bounded on its own, well under FileBytes: nothing legitimate needs
// megabytes to say what a package is called.
func TestReadRejectsAnOversizedManifest(t *testing.T) {
	limits := DefaultLimits()
	limits.ManifestBytes = 16

	_, err := Read(valid(t), limits)
	if err == nil {
		t.Fatal("a manifest over ManifestBytes was accepted")
	}

	if !strings.Contains(err.Error(), "describe a package") {
		t.Errorf("error = %q, want it to name the manifest limit", err)
	}
}

func TestLimitsAreValidOnlyWhenComplete(t *testing.T) {
	if !DefaultLimits().Valid() {
		t.Error("DefaultLimits is not valid")
	}

	if (Limits{}).Valid() {
		t.Error("a zero Limits reports itself as valid")
	}

	partial := DefaultLimits()
	partial.ManifestBytes = 0
	if partial.Valid() {
		t.Error("a Limits missing one bound reports itself as valid")
	}
}
