package attest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testFloors mirror Production with the test fixtures' values.
func testFloors() Floors {
	return Floors{
		Issuer:               DefaultIssuer,
		JWKSURL:              DefaultJWKSURL,
		SWName:               DefaultSWName,
		HWModel:              DefaultHWModel,
		ImageSignatures:      []string{testKeyID},
		MinRelease:           "v0.4.0",
		RequireStable:        true,
		RequireGpuCc:         true,
		SourceURI:            testSrcURI,
		ImageRepo:            testRepo,
		TeeSlotHostSuffixes:  []string{".tee.masseuse.ai"},
		ProjectID:            "p",
		ImageReferencePrefix: "us-central1-docker.pkg.dev/p/r/masseuse-video-tee",
	}
}

// served parses a policy the way FetchPolicy does.
func served(t *testing.T, doc string) *Policy {
	t.Helper()
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return &p
}

const fullPolicy = `{
  "allowedImageDigests": [], "allowDebug": false, "requireStable": true, "requireGpuCc": true,
  "expectedTrainerUrl": "https://masseuse-trainer.example.run.app",
  "imageSignatures": ["` + testKeyID + `"], "minRelease": "v0.4.0",
  "sourceUri": "` + testSrcURI + `", "imageRepo": "` + testRepo + `",
  "issuer": "` + DefaultIssuer + `", "jwksUrl": "` + DefaultJWKSURL + `",
  "swname": "CONFIDENTIAL_SPACE", "hwmodel": "GCP_INTEL_TDX",
  "teeSlotHostSuffixes": [".tee.masseuse.ai"]
}`

func TestFloorsApplyEqualPolicyOnlyGainsNewFields(t *testing.T) {
	f := testFloors()
	p := served(t, fullPolicy)
	before := *p
	if err := f.Apply(p); err != nil {
		t.Fatal(err)
	}
	before.ProjectID, before.ImageReferencePrefix = f.ProjectID, f.ImageReferencePrefix
	if !reflect.DeepEqual(*p, before) {
		t.Fatalf("policy changed beyond the new fields:\n got %+v\nwant %+v", *p, before)
	}
}

func TestFloorsApplyRaisesALooserPolicy(t *testing.T) {
	f := testFloors()
	// Pins digests only, allows debug, no STABLE or GPU requirement, an
	// older floor, an extra signing key, an extra host suffix, no source.
	p := served(t, `{
	  "allowedImageDigests": ["`+testDigest+`"], "allowDebug": true,
	  "imageSignatures": ["`+strings.Repeat("ee", 32)+`", "`+testKeyID+`"], "minRelease": "v0.1.0",
	  "teeSlotHostSuffixes": [".tee.masseuse.ai", ".lab.example", "a.tee.masseuse.ai"]
	}`)
	if err := f.Apply(p); err != nil {
		t.Fatal(err)
	}
	want := &Policy{
		AllowedImageDigests:  []string{testDigest},
		AllowDebug:           false,
		RequireStable:        true,
		RequireGpuCc:         true,
		ImageSignatures:      []string{testKeyID},
		MinRelease:           "v0.4.0",
		SourceURI:            testSrcURI,
		ImageRepo:            testRepo,
		Issuer:               DefaultIssuer,
		JWKSURL:              DefaultJWKSURL,
		SWName:               DefaultSWName,
		HWModel:              DefaultHWModel,
		TeeSlotHostSuffixes:  []string{".tee.masseuse.ai", "a.tee.masseuse.ai"},
		ProjectID:            "p",
		ImageReferencePrefix: "us-central1-docker.pkg.dev/p/r/masseuse-video-tee",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("raised policy:\n got %+v\nwant %+v", p, want)
	}
}

func TestFloorsApplyKeepsATighterPolicy(t *testing.T) {
	f := testFloors()
	p := served(t, `{
	  "imageSignatures": ["`+testKeyID+`"], "minRelease": "v0.5.2",
	  "sourceUri": "`+testSrcURI+`", "imageRepo": "`+testRepo+`",
	  "teeSlotHostSuffixes": [".us.tee.masseuse.ai"],
	  "projectId": "p",
	  "imageReferencePrefix": "us-central1-docker.pkg.dev/p/r/masseuse-video-tee@`+testDigest+`"
	}`)
	if err := f.Apply(p); err != nil {
		t.Fatal(err)
	}
	if p.MinRelease != "v0.5.2" || !reflect.DeepEqual(p.TeeSlotHostSuffixes, []string{".us.tee.masseuse.ai"}) ||
		p.ImageReferencePrefix != "us-central1-docker.pkg.dev/p/r/masseuse-video-tee@"+testDigest {
		t.Fatalf("tighter policy was changed: %+v", p)
	}
}

func TestFloorsApplyRefusesAContradictingPolicy(t *testing.T) {
	f := testFloors()
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"issuer", `{"imageSignatures":["` + testKeyID + `"], "issuer":"https://accounts.google.com"}`, `issuer is "https://accounts.google.com"`},
		{"jwks", `{"imageSignatures":["` + testKeyID + `"], "jwksUrl":"https://example.com/jwks"}`, "jwksUrl is"},
		{"swname", `{"imageSignatures":["` + testKeyID + `"], "swname":"OTHER"}`, "swname is"},
		{"hwmodel", `{"imageSignatures":["` + testKeyID + `"], "hwmodel":"GCP_AMD_SEV"}`, "hwmodel is"},
		{"other key only", `{"imageSignatures":["` + strings.Repeat("ee", 32) + `"]}`, "no key this build trusts"},
		{"other source", `{"imageSignatures":["` + testKeyID + `"], "sourceUri":"github.com/x/y", "imageRepo":"` + testRepo + `"}`, `sourceUri is "github.com/x/y"`},
		{"other registry", `{"imageSignatures":["` + testKeyID + `"], "sourceUri":"` + testSrcURI + `", "imageRepo":"docker.io/x/y"}`, `imageRepo is "docker.io/x/y"`},
		{"hosts elsewhere", `{"imageSignatures":["` + testKeyID + `"], "teeSlotHostSuffixes":[".lab.example", "tee.masseuse.ai.evil.example"]}`, "no entry under"},
		{"other project", `{"imageSignatures":["` + testKeyID + `"], "projectId":"q"}`, `projectId is "q"`},
		{"image elsewhere", `{"imageSignatures":["` + testKeyID + `"], "imageReferencePrefix":"us-central1-docker.pkg.dev/p/r/masseuse-video-tee-debug"}`, "is not under"},
		{"digests only, no key", `{"allowedImageDigests":["` + testDigest + `"], "issuer":"https://accounts.google.com"}`, "issuer is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := served(t, tc.doc)
			err := f.Apply(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to mention %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "policy is looser than this build's floors: ") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestFloorsApplyListsEveryContradiction(t *testing.T) {
	f := testFloors()
	p := served(t, `{"imageSignatures":["`+strings.Repeat("ee", 32)+`"], "issuer":"https://accounts.google.com", "projectId":"q"}`)
	err := f.Apply(p)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"issuer is", "no key this build trusts", "projectId is"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %q", err, want)
		}
	}
}

func TestZeroFloorsOnlyForbidDebug(t *testing.T) {
	var f Floors
	p := served(t, `{"allowedImageDigests":["`+testDigest+`"], "allowDebug": true, "issuer":"https://example.com", "jwksUrl":"https://example.com/jwks"}`)
	if err := f.Apply(p); err != nil {
		t.Fatal(err)
	}
	if p.AllowDebug || p.Issuer != "https://example.com" || p.RequireStable || len(p.ImageSignatures) != 0 {
		t.Fatalf("zero floors changed more than allowDebug: %+v", p)
	}
}

func TestProductionFloorsAcceptTheServedPolicy(t *testing.T) {
	// The policy masseuse.ai serves today (VERIFY.md), verbatim.
	p := served(t, `{
	  "allowedImageDigests": [], "allowDebug": false, "requireStable": true, "requireGpuCc": true,
	  "expectedTrainerUrl": "https://masseuse-trainer-125139120897.us-central1.run.app",
	  "imageSignatures": ["cfb085b950e93abb8332cede62fa50df662ef9aebb1533b2ae0bf1403ea4f811"],
	  "minRelease": "v0.4.0",
	  "sourceUri": "github.com/FemLed/masseuse-video-tee", "imageRepo": "ghcr.io/femled/masseuse-video-tee",
	  "issuer": "https://confidentialcomputing.googleapis.com",
	  "jwksUrl": "https://www.googleapis.com/service_accounts/v1/metadata/jwk/signer@confidentialspace-sign.iam.gserviceaccount.com",
	  "swname": "CONFIDENTIAL_SPACE", "hwmodel": "GCP_INTEL_TDX",
	  "teeSlotHostSuffixes": [".tee.masseuse.ai"]
	}`)
	if err := Production.Apply(p); err != nil {
		t.Fatal(err)
	}
	if p.ProjectID != "prod-masseuse-video-tee" || !strings.HasPrefix(p.ImageReferencePrefix, "us-central1-docker.pkg.dev/prod-masseuse-video-tee/") {
		t.Fatalf("production floors did not fill the project and registry: %+v", p)
	}
}

func TestRefUnder(t *testing.T) {
	const img = "us-central1-docker.pkg.dev/p/r/masseuse-video-tee"
	for _, tc := range []struct {
		ref, prefix string
		want        bool
	}{
		{img + "@" + testDigest, img, true},
		{img + ":v0.4.0", img, true},
		{img, img, true},
		{img + "-debug@" + testDigest, img, false},
		{img + "/x@" + testDigest, img, false},
		{"docker.io/" + img, img, false},
		{img + "@" + testDigest, "us-central1-docker.pkg.dev/p/r/", true},
		{"us-central1-docker.pkg.dev/p/other/x@" + testDigest, "us-central1-docker.pkg.dev/p/r/", false},
		{"", img, false},
	} {
		if got := refUnder(tc.ref, tc.prefix); got != tc.want {
			t.Errorf("refUnder(%q, %q) = %v, want %v", tc.ref, tc.prefix, got, tc.want)
		}
	}
}

func TestSuffixUnderAny(t *testing.T) {
	floors := []string{".tee.masseuse.ai"}
	for _, tc := range []struct {
		s    string
		want bool
	}{
		{".tee.masseuse.ai", true},
		{"tee.masseuse.ai", true},
		{".TEE.masseuse.ai.", true},
		{".us.tee.masseuse.ai", true},
		{"a.tee.masseuse.ai", true},
		{".masseuse.ai", false},
		{"tee.masseuse.ai.evil.example", false},
		{"xtee.masseuse.ai", false},
		{"", false},
	} {
		if got := suffixUnderAny(tc.s, floors); got != tc.want {
			t.Errorf("suffixUnderAny(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
