package media

// Per-protocol object shape detectors. Each detector inspects a single JSON
// object node from the walker and decides:
//
//   - claimed=true: this node is a known media-carrier shape; the walker
//     must not descend into its children (we've already produced all
//     candidates this shape can produce). The returned []candidate is
//     possibly empty (e.g. an http(s) URL we choose to skip — we still
//     claim the node so the walker doesn't think the URL string is a bare
//     base64 to decode).
//
//   - claimed=false: this object isn't a recognized media shape; the walker
//     keeps descending normally.
//
// Detector order is not load-bearing: the four supported protocols use
// disjoint field names (`image_url` / `input_audio` / `source` (with
// type=image|document) / `inlineData` / `fileData` / `inline_data` /
// `file_data`), so at most one detector can match a given object.
//
// All detectors are pure functions of (object, path); they never touch
// the filesystem or shared state. The walker takes care of side & ordering.

type shapeDetector func(obj map[string]any, path string) (claimed bool, cands []candidate)

// objectDetectors is the ordered list the walker consults. Add new
// protocols here. Each entry corresponds to one row family in Phase K § 1.
var objectDetectors = []shapeDetector{
	detectOpenAIImageURL,    // chat completions: messages[].content[].image_url
	detectOpenAIInputAudio,  // chat completions: messages[].content[].input_audio
	detectAnthropicSource,   // messages: source object inside image/document blocks
	detectGeminiInlineData,  // gemini: inlineData / inline_data
	detectGeminiFileData,    // gemini: fileData / file_data (URL-only, claimed-skip)
	detectResponsesInputImg, // openai responses: input_image with image_url
}

// --- OpenAI Chat Completions ---------------------------------------------

// detectOpenAIImageURL matches:
//
//	{"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
//	{"type": "image_url", "image_url": {"url": "https://..."}}
//
// or the bare image_url-as-object form. The carrier is the inner object;
// detection happens at that inner object so the path lands at
// `...image_url.url`.
//
// Note that the OUTER message-content object is detected by the walker as
// a generic object and we descend into its `image_url` key, at which point
// THIS detector claims the inner object.
func detectOpenAIImageURL(obj map[string]any, path string) (bool, []candidate) {
	// We're called on every object; only claim when the shape matches.
	// Shape: object has a "url" string and the path ends with ".image_url"
	// (i.e. we are the value of an image_url key). This catches both the
	// chat form and any nested image_url object form.
	if !endsWith(path, ".image_url") && path != "image_url" {
		return false, nil
	}
	urlVal, ok := obj["url"].(string)
	if !ok || urlVal == "" {
		// Shape matched the name but not the structure. Claim it anyway so
		// we don't accidentally descend and double-extract — there's no
		// useful media here.
		return true, nil
	}
	return true, []candidate{{
		path:    path + ".url",
		payload: urlVal,
		// MIME comes from the data:URL prefix if any; for https:// the
		// decoder will see no data: prefix and skip the URL silently.
		declaredMime: "",
	}}
}

// detectOpenAIInputAudio matches:
//
//	{"type":"input_audio","input_audio":{"data":"<b64>","format":"wav"}}
//
// Called on the inner input_audio object (path ends with ".input_audio").
func detectOpenAIInputAudio(obj map[string]any, path string) (bool, []candidate) {
	if !endsWith(path, ".input_audio") && path != "input_audio" {
		return false, nil
	}
	data, ok := obj["data"].(string)
	if !ok || data == "" {
		return true, nil
	}
	// "format" is wav / mp3 / ...; turn it into a mime type.
	mime := ""
	if f, ok := obj["format"].(string); ok && f != "" {
		mime = "audio/" + f
	}
	return true, []candidate{{
		path:         path + ".data",
		payload:      data,
		declaredMime: mime,
	}}
}

// --- Anthropic Messages --------------------------------------------------

// detectAnthropicSource matches the `source` sub-object that appears inside
// an Anthropic image or document content block:
//
//	{"type":"base64","media_type":"image/png","data":"..."}
//	{"type":"url","url":"https://..."}
//	{"type":"text","data":"..."} (plaintext docs; skipped per § 2)
//	{"type":"content", ...}      (nested; skipped — not a binary blob)
//
// Triggered when path ends with ".source" and the object has a "type"
// discriminator that places it in the Anthropic shape.
func detectAnthropicSource(obj map[string]any, path string) (bool, []candidate) {
	if !endsWith(path, ".source") && path != "source" {
		return false, nil
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "base64":
		data, ok := obj["data"].(string)
		if !ok || data == "" {
			return true, nil
		}
		mime, _ := obj["media_type"].(string)
		return true, []candidate{{
			path:         path + ".data",
			payload:      data,
			declaredMime: mime,
		}}
	case "url":
		// URL-only; claim to prevent the walker from misreading the URL
		// string as a bare-base64 elsewhere. No candidate produced.
		return true, nil
	case "text", "content":
		// Plaintext document content or nested Parts; not a binary blob,
		// not extractable. Claim and skip.
		return true, nil
	default:
		// Unknown type discriminator: don't claim — let the walker descend
		// in case a future protocol revision adds new shapes we'd want to
		// pick up generically. (Defensive; PHILOSOPHY § 1 says render
		// what's there, so descending costs us nothing.)
		return false, nil
	}
}

// --- Google Gemini -------------------------------------------------------

// detectGeminiInlineData matches:
//
//	{"mimeType":"image/png","data":"<b64>"}    (camelCase, native)
//	{"mime_type":"image/png","data":"<b64>"}   (snake_case, some SDKs)
//
// Triggered by path ending in `.inlineData` or `.inline_data`. We accept
// both casings because the Gemini SDKs in the wild use either.
func detectGeminiInlineData(obj map[string]any, path string) (bool, []candidate) {
	if !endsWith(path, ".inlineData") && !endsWith(path, ".inline_data") &&
		path != "inlineData" && path != "inline_data" {
		return false, nil
	}
	data, ok := obj["data"].(string)
	if !ok || data == "" {
		return true, nil
	}
	mime, _ := obj["mimeType"].(string)
	if mime == "" {
		mime, _ = obj["mime_type"].(string)
	}
	return true, []candidate{{
		path:         path + ".data",
		payload:      data,
		declaredMime: mime,
	}}
}

// detectGeminiFileData matches:
//
//	{"mimeType":"image/png","fileUri":"gs://bucket/path"}
//	{"mime_type":"image/png","file_uri":"gs://..."}
//
// File-data is URL-only — the actual bytes live on Google's side, we have
// no local content. Claim the shape (so the URI string isn't reinterpreted
// downstream) but emit no candidate.
func detectGeminiFileData(obj map[string]any, path string) (bool, []candidate) {
	if !endsWith(path, ".fileData") && !endsWith(path, ".file_data") &&
		path != "fileData" && path != "file_data" {
		return false, nil
	}
	return true, nil
}

// --- OpenAI Responses ----------------------------------------------------

// detectResponsesInputImg matches an `input_image` content block from the
// /v1/responses protocol:
//
//	{"type":"input_image","image_url":"https://..."}        (bare string)
//	{"type":"input_image","image_url":{"url":"https://..."}} (object form)
//
// The bare-string form is the one that doesn't fall through to
// detectOpenAIImageURL (since there's no inner object to descend into).
// The URL form is URL-only per Phase K § 1.3 — we never fetch remote
// images. Claim and skip in both cases.
//
// Triggered by an object that has type=="input_image".
func detectResponsesInputImg(obj map[string]any, path string) (bool, []candidate) {
	typ, _ := obj["type"].(string)
	if typ != "input_image" {
		return false, nil
	}
	// We don't extract; remote URL is out of scope per Phase K § 5.2. We
	// still claim so the walker doesn't dive into the URL string as if it
	// were base64.
	_ = path
	return true, nil
}

// --- helpers -------------------------------------------------------------

// endsWith returns true if path ends with suffix. We use a tiny helper
// rather than strings.HasSuffix to keep imports minimal in this file and
// signal intent (we're matching JSON-path suffixes, not arbitrary strings).
func endsWith(path, suffix string) bool {
	if len(path) < len(suffix) {
		return false
	}
	return path[len(path)-len(suffix):] == suffix
}
