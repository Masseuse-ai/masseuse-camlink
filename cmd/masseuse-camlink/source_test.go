package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSourceConfigKeepsEachPartUnlessSpoken(t *testing.T) {
	dir := t.TempDir()
	// Nothing saved, no flags: the computer's first camera and microphone,
	// no front-facing camera, the phone's picture not wanted.
	cfg, err := resolveSourceConfig(dir, sourceFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "capture" || cfg.FaceCamera != "" || cfg.SharePhone {
		t.Fatalf("defaults %+v", cfg)
	}
	// The camera chosen and remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{camera: "1", mic: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// A front-facing camera added to it, and the phone's picture asked for:
	// the camera part is as remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceCamera: "OBS Virtual Camera", sharePhone: "on"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "capture" || cfg.Camera != "1" || cfg.Mic != "0" || cfg.FaceCamera != "OBS Virtual Camera" || !cfg.SharePhone {
		t.Fatalf("added: %+v", cfg)
	}
	cfg.ShareSecret = "s3cret"
	cfg.SharePort = 7446
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// A new camera on the network replaces the camera part alone: the face
	// and the share, secret included, stay.
	cfg, err = resolveSourceConfig(dir, sourceFlags{cameraURL: "rtsps://cam.local:322/live"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != "camera" || cfg.URL != "rtsps://cam.local:322/live" || cfg.Camera != "" ||
		cfg.FaceCamera != "OBS Virtual Camera" || !cfg.SharePhone || cfg.ShareSecret != "s3cret" || cfg.SharePort != 7446 {
		t.Fatalf("replaced camera: %+v", cfg)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	// The face's encoding alone, with a face remembered.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceFPS: 24, faceBitrate: "1500k"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FaceCamera != "OBS Virtual Camera" || cfg.FaceFPS != 24 || cfg.FaceBitrate != "1500k" {
		t.Fatalf("face encoding: %+v", cfg)
	}
	// `-face-camera none` takes the front-facing camera away; the share
	// stays until told off.
	cfg, err = resolveSourceConfig(dir, sourceFlags{faceCamera: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FaceCamera != "" || cfg.FaceFPS != 0 || !cfg.SharePhone || cfg.Kind != "camera" {
		t.Fatalf("face none: %+v", cfg)
	}
	if err := saveSourceConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = resolveSourceConfig(dir, sourceFlags{sharePhone: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SharePhone || cfg.ShareSecret != "s3cret" {
		t.Fatalf("share off keeps the secret for next time: %+v", cfg)
	}
	// What is refused.
	for _, bad := range []sourceFlags{
		{faceCamera: "none", faceFPS: 30},
		{faceFPS: 30}, // no face remembered now
		{sharePhone: "maybe"},
		{sharePort: 70000},
		{cameraURL: "rtsps://x/live", camera: "0"},
		{cameraFingerprint: "ab"},
	} {
		if _, err := resolveSourceConfig(dir, bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	// The saved file carries the new fields by their names.
	b, err := os.ReadFile(filepath.Join(dir, sourceFile))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["sharePhone"] != true || raw["shareSecret"] != "s3cret" || raw["kind"] != "camera" {
		t.Fatalf("saved %v", raw)
	}
}
