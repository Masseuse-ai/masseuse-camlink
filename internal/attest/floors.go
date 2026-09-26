package attest

import (
	"fmt"
	"strings"
)

// Floors are the parts of the enclave policy this build of the connector
// holds on its own. The service publishes the policy at
// <service>/api/tee-policy and the connector applies it, but a policy is
// only as trustworthy as the server that serves it: a server that could
// serve a looser one (another token issuer, another signing key, a debug
// image, a host outside the enclave fleet) could point a camera at an
// enclave of its choosing. So the served policy may only tighten what the
// floors say, and the floors change only with a release of the connector,
// which is reproducible and signed (VERIFY.md).
//
// Apply raises a served policy to the floors where it is looser, and
// refuses one that contradicts them. Every field is optional; the zero
// value only forbids debug images.
type Floors struct {
	// The token's issuer, key set, software and hardware: a served value
	// must be equal.
	Issuer  string
	JWKSURL string
	SWName  string
	HWModel string
	// ImageSignatures are the key ids the connector trusts to have signed
	// the image. A served list is cut down to those; an empty served list
	// becomes this one; a served list with none of them is refused.
	ImageSignatures []string
	// MinRelease is the lowest release an enclave may run; a served floor
	// below it (or none) is raised to it.
	MinRelease string
	// RequireStable and RequireGpuCc, when set, are forced on. Debug images
	// are never allowed under floors.
	RequireStable bool
	RequireGpuCc  bool
	// SourceURI and ImageRepo name where the image's provenance is checked;
	// a served pair must be equal, an absent one is filled in.
	SourceURI string
	ImageRepo string
	// TeeSlotHostSuffixes are the DNS suffixes enclaves live under. Served
	// suffixes are kept only when they fall under one of these; none left
	// is refused; an empty served list becomes this one.
	TeeSlotHostSuffixes []string
	// ProjectID is the Google Cloud project the enclave VM must run in
	// (submods.gce.project_id); ImageReferencePrefix is the registry path
	// the launcher must have pulled the image from
	// (submods.container.image_reference, before its @digest or :tag). A
	// served value must be equal, or under the prefix; absent ones are
	// filled in.
	ProjectID            string
	ImageReferencePrefix string
}

// Production is the floor for masseuse.ai enclaves: Confidential Space on
// Intel TDX, images signed by the masseuse-video-tee release workflow's key
// and released at v0.4.0 or later, running in that repository's project
// from its registry, reachable under .tee.masseuse.ai. The key id is the
// hex SHA-256 of the release signing key's DER public key, the value the
// launcher reports in image_signatures[].key_id (masseuse-video-tee,
// VERIFY.md).
var Production = Floors{
	Issuer:               DefaultIssuer,
	JWKSURL:              DefaultJWKSURL,
	SWName:               DefaultSWName,
	HWModel:              DefaultHWModel,
	ImageSignatures:      []string{"cfb085b950e93abb8332cede62fa50df662ef9aebb1533b2ae0bf1403ea4f811"},
	MinRelease:           "v0.4.0",
	RequireStable:        true,
	RequireGpuCc:         true,
	SourceURI:            "github.com/FemLed/masseuse-video-tee",
	ImageRepo:            "ghcr.io/femled/masseuse-video-tee",
	TeeSlotHostSuffixes:  []string{".tee.masseuse.ai"},
	ProjectID:            "prod-masseuse-video-tee",
	ImageReferencePrefix: "us-central1-docker.pkg.dev/prod-masseuse-video-tee/masseuse-video-tee/masseuse-video-tee",
}

// Apply tightens a validated policy to the floors in place. It returns an
// error naming every way the policy contradicts them, in which case the
// policy must not be used.
func (f *Floors) Apply(p *Policy) error {
	var reasons []string
	same := func(name, got, want string) {
		if want != "" && got != want {
			reasons = append(reasons, fmt.Sprintf("%s is %q, this build expects %q", name, got, want))
		}
	}
	same("issuer", p.Issuer, f.Issuer)
	same("jwksUrl", p.JWKSURL, f.JWKSURL)
	same("swname", p.SWName, f.SWName)
	same("hwmodel", p.HWModel, f.HWModel)

	if len(f.ImageSignatures) > 0 {
		if len(p.ImageSignatures) == 0 {
			p.ImageSignatures = append([]string(nil), f.ImageSignatures...)
		} else {
			var kept []string
			for _, k := range p.ImageSignatures {
				if contains(f.ImageSignatures, k) {
					kept = append(kept, k)
				}
			}
			if len(kept) == 0 {
				reasons = append(reasons, "imageSignatures names no key this build trusts")
			} else {
				p.ImageSignatures = kept
			}
		}
	}
	if f.MinRelease != "" && (p.MinRelease == "" || CompareRelease(p.MinRelease, f.MinRelease) < 0) {
		p.MinRelease = f.MinRelease
	}
	p.AllowDebug = false
	p.RequireStable = p.RequireStable || f.RequireStable
	p.RequireGpuCc = p.RequireGpuCc || f.RequireGpuCc

	if f.SourceURI != "" && f.ImageRepo != "" {
		if p.SourceURI == "" && p.ImageRepo == "" {
			p.SourceURI, p.ImageRepo = f.SourceURI, f.ImageRepo
		} else {
			same("sourceUri", p.SourceURI, f.SourceURI)
			same("imageRepo", p.ImageRepo, f.ImageRepo)
		}
	}
	if len(f.TeeSlotHostSuffixes) > 0 {
		if len(p.TeeSlotHostSuffixes) == 0 {
			p.TeeSlotHostSuffixes = append([]string(nil), f.TeeSlotHostSuffixes...)
		} else {
			var kept []string
			for _, s := range p.TeeSlotHostSuffixes {
				if suffixUnderAny(s, f.TeeSlotHostSuffixes) {
					kept = append(kept, s)
				}
			}
			if len(kept) == 0 {
				reasons = append(reasons, fmt.Sprintf("teeSlotHostSuffixes %v has no entry under %v", p.TeeSlotHostSuffixes, f.TeeSlotHostSuffixes))
			} else {
				p.TeeSlotHostSuffixes = kept
			}
		}
	}
	if f.ProjectID != "" {
		if p.ProjectID == "" {
			p.ProjectID = f.ProjectID
		} else {
			same("projectId", p.ProjectID, f.ProjectID)
		}
	}
	if f.ImageReferencePrefix != "" {
		switch {
		case p.ImageReferencePrefix == "":
			p.ImageReferencePrefix = f.ImageReferencePrefix
		case !refUnder(p.ImageReferencePrefix, f.ImageReferencePrefix):
			reasons = append(reasons, fmt.Sprintf("imageReferencePrefix %q is not under %q", p.ImageReferencePrefix, f.ImageReferencePrefix))
		}
	}
	if len(reasons) > 0 {
		return fmt.Errorf("policy is looser than this build's floors: %s", strings.Join(reasons, "; "))
	}
	return nil
}

// suffixUnderAny reports whether host suffix s names the same or a
// narrower set of hosts than one of floors: ".a.tee.example" is under
// ".tee.example", and so is ".tee.example" itself.
func suffixUnderAny(s string, floors []string) bool {
	norm := func(x string) string {
		x = strings.ToLower(strings.TrimSuffix(x, "."))
		if x != "" && !strings.HasPrefix(x, ".") {
			x = "." + x
		}
		return x
	}
	s = norm(s)
	if s == "" {
		return false
	}
	for _, fl := range floors {
		if fl = norm(fl); fl != "" && strings.HasSuffix(s, fl) {
			return true
		}
	}
	return false
}

// refUnder reports whether an image reference (or a reference prefix) is
// the image at prefix: equal to it, or it followed by "@digest" or ":tag".
// A prefix ending in "/" is a registry path and matches any image under it.
func refUnder(ref, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(ref, prefix)
	}
	return ref == prefix || strings.HasPrefix(ref, prefix+"@") || strings.HasPrefix(ref, prefix+":")
}
