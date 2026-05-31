package main

// Pinned viewer version + SHA — see RELEASING.md "Hosted viewer
// version + SHA bump" for the bump procedure.
//
// Trust model: the backend SOURCE pins both the viewer version and
// the expected SHA-256 of the dist.zip release asset. The placeholder
// SHA is intentionally all zeros so a binary built before the real
// SHA is filled in will sha-mismatch and 503 the `/viewer/` route,
// instead of silently serving something unverified.
//
// To bump: follow RELEASING.md §"Hosted viewer — version + SHA bump"
// — wait for the viewer release job to publish `dist.zip` +
// `dist.zip.sha256`, copy the 64-char hex into viewerSha256, set
// viewerVersion to the matching tag, commit, then cut the api-log
// release.
const (
	viewerVersion = "v0.1.0"
	viewerSha256  = "0000000000000000000000000000000000000000000000000000000000000000" // placeholder — replaced at release ceremony
	viewerRepo    = "2nd1st/api-log-viewer"
)
