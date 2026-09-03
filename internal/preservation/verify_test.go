package preservation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyExportChecksumsCountsAndReferences(t *testing.T) {
	root := makeTestExport(t)
	manifest, err := VerifyExport(context.Background(), root)
	if err != nil {
		t.Fatalf("VerifyExport: %v", err)
	}
	if manifest.Counts.Messages != 1 || manifest.Counts.Blobs != 1 {
		t.Fatalf("manifest counts = %+v", manifest.Counts)
	}

	if err := os.WriteFile(filepath.Join(root, "messages.jsonl"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered VerifyExport error = %v", err)
	}
}

func TestVerifyExportRejectsBrokenReferenceEvenWithValidChecksums(t *testing.T) {
	root := makeTestExport(t)
	messagePath := filepath.Join(root, "messages.jsonl")
	message := `{"id":"m1","channel_id":"missing","actor_id":"a1","reply_to_message_id":null}` + "\n"
	if err := os.WriteFile(messagePath, []byte(message), 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := readChecksums(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	sums["messages.jsonl"], err = hashFile(messagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "checksums.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(filepath.Join(root, "checksums.sha256"), sums); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "missing channel") {
		t.Fatalf("broken reference error = %v", err)
	}
}

func TestVerifyExportRequiresDeclaredMarkdownRendering(t *testing.T) {
	root := makeTestExport(t)
	manifestPath := filepath.Join(root, "manifest.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Renderings = []string{"markdown"}
	manifestBody, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBody = append(manifestBody, '\n')
	if err := os.WriteFile(manifestPath, manifestBody, 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := readChecksums(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	sums["manifest.json"] = hashBytes(manifestBody)
	if err := os.Remove(filepath.Join(root, "checksums.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(filepath.Join(root, "checksums.sha256"), sums); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "no checksummed index") {
		t.Fatalf("missing Markdown rendering error = %v", err)
	}
}

func makeTestExport(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	blobBody := []byte("preserved bytes")
	blobHash := sha256.Sum256(blobBody)
	digest := hex.EncodeToString(blobHash[:])
	records := map[string]string{
		"communities.jsonl":     `{"id":"c1","source":"discord","external_id":"guild","name":"Guild"}` + "\n",
		"channels.jsonl":        `{"id":"ch1","community_id":"c1","parent_channel_id":null}` + "\n",
		"actors.jsonl":          `{"id":"a1","source":"discord","external_id":"user"}` + "\n",
		"messages.jsonl":        `{"id":"m1","channel_id":"ch1","actor_id":"a1","reply_to_message_id":null}` + "\n",
		"attachments.jsonl":     `{"id":"att1","message_id":"m1","download_status":"stored","sha256":"` + digest + `"}` + "\n",
		"bookmarks.jsonl":       `{"id":"b1","message_id":"m1"}` + "\n",
		"deletion_ledger.jsonl": "",
	}
	manifest := Manifest{
		Format: FormatName, FormatVersion: FormatVersion, GeneratedAt: time.Unix(1, 0).UTC(),
		OpenConvoVersion: "test", Sources: []string{"discord"},
		Communities: []ManifestCommunity{{ID: "c1", Source: "discord", ExternalID: "guild", Name: "Guild"}},
		Counts:      ManifestCounts{Communities: 1, Channels: 1, Actors: 1, Messages: 1, Attachments: 1, Bookmarks: 1, Blobs: 1},
	}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	records["manifest.json"] = string(manifestBody) + "\n"
	for name, body := range records {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	blobRel := filepath.ToSlash(filepath.Join("blobs", "sha256", digest[:2], digest))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(blobRel))), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(blobRel)), blobBody, 0o644); err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	for name := range records {
		sums[name], err = hashFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
	}
	sums[blobRel] = digest
	if err := writeChecksums(filepath.Join(root, "checksums.sha256"), sums); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestVerifyExportAcceptsDocumentedReactionShape(t *testing.T) {
	root := makeTestExport(t)
	// Exactly the reaction object published in docs/archive-format.md: no
	// row id, no message_id.
	documented := `{"id":"m1","channel_id":"ch1","actor_id":"a1","reply_to_message_id":null,` +
		`"reactions":[{"emoji_key":"👍","emoji_name":"👍","count":3}]}` + "\n"
	rewriteExportFile(t, root, "messages.jsonl", documented)
	if _, err := VerifyExport(context.Background(), root); err != nil {
		t.Fatalf("documented reaction shape failed verification: %v", err)
	}

	misattributed := `{"id":"m1","channel_id":"ch1","actor_id":"a1","reply_to_message_id":null,` +
		`"reactions":[{"emoji_key":"👍","message_id":"m2","count":3}]}` + "\n"
	rewriteExportFile(t, root, "messages.jsonl", misattributed)
	if _, err := VerifyExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "belonging to message m2") {
		t.Fatalf("misattributed reaction error = %v", err)
	}
}

func TestVerifyExportRejectsBlobThatDoesNotMatchItsPath(t *testing.T) {
	root := makeTestExport(t)
	sums, err := readChecksums(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	var blobPath string
	for name := range sums {
		if strings.HasPrefix(name, "blobs/") {
			blobPath = name
		}
	}
	if blobPath == "" {
		t.Fatal("test export contains no blob")
	}
	// The operator "repairs" a failed verify by regenerating
	// checksums.sha256 with sha256sum: every checksum line now agrees with
	// the corrupted bytes, and only the content address disagrees.
	rewriteExportFile(t, root, blobPath, "bytes that are not the addressed content")
	if _, err := VerifyExport(context.Background(), root); err == nil || !strings.Contains(err.Error(), "recorded with checksum") {
		t.Fatalf("laundered blob error = %v", err)
	}
}

// rewriteExportFile replaces one export file and repairs its checksum line,
// leaving the rest of the export untouched.
func rewriteExportFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := readChecksums(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if sums[name], err = hashFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "checksums.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(filepath.Join(root, "checksums.sha256"), sums); err != nil {
		t.Fatal(err)
	}
}
