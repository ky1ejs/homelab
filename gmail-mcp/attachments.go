package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Attachments on outgoing mail.
//
// A remote MCP client cannot point this server at a file on its own disk, so a
// file reaches it one of two ways:
//
//   - inline, as base64 in the tool call. Every byte of that is a token the
//     model has to write, so it suits a small file the model made itself (a
//     .csv, an .ics) and nothing larger than maxInlineBytes.
//   - by upload: create_upload returns a one-time URL, the client PUTs the
//     file there from wherever it lives (curl -T from an agent's sandbox), and
//     the tool call names the upload. A client that cannot reach the URL
//     fills the same upload over MCP with append_upload_chunk. See uploads.go.
//
// Either way the bytes are checked here before they go anywhere, and the
// message is built by build() with every part base64-encoded.

const (
	// Gmail's own limit on attachments, decoded. The encoded message is about
	// a third larger and goes up through the media upload endpoint, whose
	// limit (35 MB) is above that; see gmailClient.upload.
	maxAttachmentBytes = 25 << 20
	// Decoded bytes across all inline attachments in one call. Base64 makes
	// that about 2.7 MB of JSON, inside the SDK's 4 MiB request limit, and
	// far more than a model should ever be asked to write out.
	maxInlineBytes = 2 << 20
	maxAttachments = 10
	maxFilename    = 255 // bytes; keeps the encoded header under 998 characters
)

type outgoingAttachment struct {
	Filename string
	MimeType string
	Data     []byte
}

// attachmentSummary is what a draft or send reports back for each file, so
// the caller, and a person reading the result, can check what went.
type attachmentSummary struct {
	Filename string `json:"filename"`
	MimeType string `json:"mime_type"`
	Size     int    `json:"size"`
	SHA256   string `json:"sha256"`
}

func summarize(a outgoingAttachment) attachmentSummary {
	sum := sha256.Sum256(a.Data)
	return attachmentSummary{Filename: a.Filename, MimeType: a.MimeType, Size: len(a.Data), SHA256: hex.EncodeToString(sum[:])}
}

// Gmail refuses mail carrying these, even zipped, and says only "message
// rejected". Refusing them here says why, before anything is sent. From
// https://support.google.com/mail/answer/6590.
var blockedExtensions = map[string]bool{
	".ade": true, ".adp": true, ".apk": true, ".appx": true, ".appxbundle": true,
	".bat": true, ".cab": true, ".chm": true, ".cmd": true, ".com": true,
	".cpl": true, ".diagcab": true, ".diagcfg": true, ".diagpack": true, ".dll": true,
	".dmg": true, ".ex": true, ".ex_": true, ".exe": true, ".hta": true,
	".img": true, ".ins": true, ".iso": true, ".isp": true, ".jar": true,
	".jnlp": true, ".js": true, ".jse": true, ".lib": true, ".lnk": true,
	".mde": true, ".mjs": true, ".msc": true, ".msi": true, ".msix": true,
	".msixbundle": true, ".msp": true, ".mst": true, ".nsh": true, ".pif": true,
	".ps1": true, ".scr": true, ".sct": true, ".shb": true, ".sys": true,
	".vb": true, ".vbe": true, ".vbs": true, ".vhd": true, ".vxd": true,
	".wsc": true, ".wsf": true, ".wsh": true, ".xll": true,
}

// checkFilename accepts a name a mail client can show and save, and nothing
// that could end a header line or name a path. Non-ASCII is fine: it goes out
// RFC 2231-encoded, never raw.
func checkFilename(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: an attachment needs a filename", errBadInput)
	case len(name) > maxFilename:
		return fmt.Errorf("%w: attachment filename is over %d bytes", errBadInput, maxFilename)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: attachment filename is not valid UTF-8", errBadInput)
	case name == "." || name == ".." || strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: %q is not a usable filename", errBadInput, name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '/' || r == '\\' || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("%w: attachment filename %q has a control character or a path separator", errBadInput, name)
		}
		// Format characters are invisible: every bidi control (the overrides
		// make "photo\u202egpj.exe" display as photoexe.jpg, and U+061C is
		// one too), zero-width spaces and joiners, the BOM and the soft
		// hyphen. Any of them lets a name look like another.
		if unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: attachment filename %q has an invisible or text-direction character", errBadInput, name)
		}
		// Wide or odd spaces can push the real extension out of a truncated
		// attachment chip: "invoice.pdf\u3000\u3000.html". Plain spaces are fine.
		if r != ' ' && unicode.IsSpace(r) || unicode.Is(unicode.Zs, r) && r != ' ' {
			return fmt.Errorf("%w: attachment filename %q has a space character other than a plain space", errBadInput, name)
		}
	}
	// Windows drops a trailing dot when it saves: "report.exe." lands as
	// report.exe, past the extension check below.
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("%w: attachment filename %q ends with a dot", errBadInput, name)
	}
	if ext := strings.ToLower(path.Ext(name)); blockedExtensions[ext] {
		return fmt.Errorf("%w: Gmail does not allow %s attachments, even zipped", errBadInput, ext)
	}
	return nil
}

// A bare type/subtype, lower case, with no parameters: a parameter is where a
// second boundary or a charset trick would go, and an attachment needs none.
var mimeTypeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)

// attachmentType returns the type to label a part with: the one given, checked,
// or a guess from the extension.
func attachmentType(given, filename string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(given))
	if t == "" {
		t = "application/octet-stream"
		if guess, _, err := mime.ParseMediaType(mime.TypeByExtension(path.Ext(filename))); err == nil && mimeTypeRe.MatchString(guess) {
			t = guess
		}
	}
	if !mimeTypeRe.MatchString(t) {
		return "", fmt.Errorf("%w: %q is not a MIME type such as image/jpeg", errBadInput, given)
	}
	// multipart/* would claim a structure the part does not have, and
	// RFC 2046 forbids base64 for message/*. Neither is a file.
	if strings.HasPrefix(t, "multipart/") || strings.HasPrefix(t, "message/") {
		return "", fmt.Errorf("%w: %s cannot be sent as an attachment", errBadInput, t)
	}
	return t, nil
}

// The types http.DetectContentType recognises reliably by their first bytes.
// For these a name and type are a claim the bytes can contradict. For
// anything else (a .docx, a .heic, a .csv) the sniffer has nothing useful to
// say, so the bytes are not judged.
func sniffable(t string) bool {
	switch t {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "application/pdf":
		return true
	}
	return false
}

// checkContent refuses bytes that plainly are not what their name or type
// says: HTML, a zip or a script under photo.jpg, say. A steered model, or
// whoever filled an upload, could otherwise send a lure under a harmless name.
// Both the declared type and the one the extension implies are checked,
// because mail clients go by either.
func checkContent(filename, declared string, data []byte) error {
	detected, _, _ := strings.Cut(http.DetectContentType(data), ";")
	byExt, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(path.Ext(filename))), ";")
	for _, claim := range []string{declared, strings.ToLower(byExt)} {
		if sniffable(claim) && detected != claim {
			return fmt.Errorf("%w: %q is labelled %s, but its bytes look like %s", errBadInput, filename, claim, detected)
		}
	}
	return nil
}

// decodeInline accepts standard or URL-safe base64, padded or not, with line
// breaks or spaces anywhere, since that is how a model or a tool wraps it.
func decodeInline(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// writeAttachment appends one base64 part. The boundary line is written by the
// caller.
func writeAttachment(b *bytes.Buffer, a outgoingAttachment) error {
	ct := mime.FormatMediaType(a.MimeType, map[string]string{"name": a.Filename})
	cd := mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})
	if ct == "" || cd == "" {
		return fmt.Errorf("%w: attachment %q cannot be labelled", errBadInput, a.Filename)
	}
	b.WriteString("Content-Type: " + ct + "\r\n")
	b.WriteString("Content-Disposition: " + cd + "\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	writeBase64Lines(b, a.Data)
	return nil
}

// writeBase64Lines writes data as base64 in 76-character lines (RFC 2045),
// without holding a second full copy of the encoding.
func writeBase64Lines(b *bytes.Buffer, data []byte) {
	const perLine = 57 // input bytes per 76-character line
	line := make([]byte, base64.StdEncoding.EncodedLen(perLine))
	for len(data) > 0 {
		n := min(perLine, len(data))
		base64.StdEncoding.Encode(line, data[:n])
		b.Write(line[:base64.StdEncoding.EncodedLen(n)])
		b.WriteString("\r\n")
		data = data[n:]
	}
}
