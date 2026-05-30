// Package v2 is the BUILD-phase hot-path plugin contract described in
// uiux-research/plugin-b-c-spec.md.
//
// It coexists with the Phase A observe-class scaffold in
// github.com/leoyun/api-log/internal/plugin. The Phase A package is not
// touched by W1; W6 migrates it. Until then, both packages are buildable
// independent surfaces.
//
// The v2 surface is the surviving BEFORE / AFTER hook class ratified by
// operator 2026-05-30. The "interfere class" is bounded by two call
// sites (post-receive / pre-forward and post-upstream / pre-client-send),
// honored fail-open per spec §4, and recorded post-mutation only per
// spec §5. v1 does NOT carry tool_call argument mutation on the AFTER
// hook (spec §10.6 carve-out); streaming tool_use input_json_delta
// events pass through untouched.
package v2
