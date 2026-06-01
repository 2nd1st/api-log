// Package v2 implements the hot-path BEFORE / AFTER plugin contract.
//
// It coexists with the observe-class plugin scaffold under
// github.com/2nd1st/api-log/internal/plugin; both packages are
// buildable independent surfaces.
//
// The "interfere class" is bounded by two call sites (post-receive /
// pre-forward and post-upstream / pre-client-send), honored fail-open
// per the spec, and recorded post-mutation only. The current contract
// does NOT carry tool_call argument mutation on the AFTER hook;
// streaming tool_use input_json_delta events pass through untouched.
package v2
