package main

// Pinned viewer version + SHA — see RELEASING.md "Hosted viewer
// version + SHA bump" for the bump procedure.
//
// Trust model: the backend SOURCE pins both the viewer version and
// the expected SHA-256 of the dist.zip release asset. To tamper with
// the served viewer an attacker would need to either compromise the
// backend source (caught at code review) or defeat GitHub's release-
// asset hash (out of scope; same trust GitHub itself).
//
// To bump: follow RELEASING.md §"Hosted viewer — version + SHA bump"
// — wait for the viewer release job to publish `dist.zip` +
// `dist.zip.sha256`, copy the 64-char hex into viewerSha256, set
// viewerVersion to the matching tag, commit, then cut the api-log
// release.
const (
	viewerVersion = "v0.1.2"
	viewerSha256  = "8ed5c40fc6a467ad5015fb2b4496fe4499aed7ab1c69262edbc9b49921100bf2"
	viewerRepo    = "2nd1st/api-log-viewer"
)
