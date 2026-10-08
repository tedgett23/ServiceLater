package storage

import (
	"mime"
	"strings"
)

// inlineTypes are the only files shown in the browser rather than saved: pictures that cannot run script. Anything
// else, HTML, SVG and PDF included, is always downloaded.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true}

// Inline reports whether a file of this type may be shown in the browser.
func Inline(contentType string) bool { return inlineTypes[strings.ToLower(contentType)] }

// Headers are the Content-Type and Content-Disposition a file is served with: shown only when asked and safe to show,
// else saved under its name; a type that may not be shown is sent as plain bytes.
func Headers(filename, contentType string, inline bool) (ct, disposition string) {
	ct = "application/octet-stream"
	if Inline(contentType) {
		ct = strings.ToLower(contentType)
	} else {
		inline = false
	}
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	disposition = mime.FormatMediaType(kind, map[string]string{"filename": filename})
	if disposition == "" { // a name FormatMediaType cannot encode
		disposition = kind
	}
	return ct, disposition
}

// ContentType is the type a file is stored with: what the browser said when it looks like a media type, else
// application/octet-stream.
func ContentType(declared string) string {
	t, _, err := mime.ParseMediaType(strings.TrimSpace(declared))
	if err != nil || !strings.Contains(t, "/") || len(t) > 255 {
		return "application/octet-stream"
	}
	return t
}
