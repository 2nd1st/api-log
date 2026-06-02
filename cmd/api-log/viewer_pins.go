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
	viewerVersion = "v0.1.1"
	viewerSha256  = "03065bf988eb78c9b467be1effd5b6e6a1026e18d3c1e52799a04b3c20cfe2e9"
	viewerRepo    = "2nd1st/api-log-viewer"
)
