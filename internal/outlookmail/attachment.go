package outlookmail

import (
	"path"
	"strings"

	"github.com/pA1nD/hxmail/internal/hxstore"
)

// Attachment fixed region (class 0x16a).
const (
	atSize      = 568
	atName      = 608
	atState     = 624
	atPath      = 648
	atMessage   = 380
	atContentID = 576
	// atInline is 2 on the 58 attachments the server reports Inline=1 for and 0 on the one it
	// reports Inline=0 for (Osa sync log, 16.114). A Content-ID alone is no sign: Gmail sets one
	// on ordinary attachments too.
	atInline = 740

	// downloadedState is the value at +624 of an attachment whose file exists (likely: it agreed
	// with file presence on every attachment of a measured copy).
	downloadedState = 2
)

// Attachment is an attachment record. Inline marks an embedded part (an image in the HTML body),
// as the byte at +740 says.
type Attachment struct {
	Key         uint32
	MessageKey  uint32 // the detail key of the message
	Name        string
	Size        int64
	ContentType string
	Inline      bool
	Downloaded  bool
	ContentID   string // the MIME Content-ID; also set on ordinary Gmail attachments
	Path        string // ~/Files/... under the profile, empty when the store names none
	// BadStrings counts string fields that were present but unreadable and were left blank.
	BadStrings int
}

// MapAttachment maps a class 0x16a object.
func MapAttachment(o hxstore.Object) (Attachment, error) {
	f, err := newFields(o, TagAttachment)
	if err != nil {
		return Attachment{}, err
	}
	a := Attachment{
		Key: word(o, offKey), MessageKey: word(o, atMessage), Size: int64(word(o, atSize)),
		Name: f.text(atName), Path: f.text(atPath), Downloaded: word(o, atState) == downloadedState,
	}
	a.ContentID = f.text(atContentID)
	a.BadStrings = f.bad
	// The inline byte decides. Upstream also took any bare-GUID name for inline, which would
	// drop an ordinary attachment that happens to have such a name.
	inline, _ := o.U8(atInline)
	a.ContentType, a.Inline = contentType(a.Name), inline == 2
	return a, nil
}

var contentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".ics": "text/calendar", ".pdf": "application/pdf", ".txt": "text/plain", ".zip": "application/zip",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// contentType gives the type from a fixed table of extensions, else application/octet-stream.
func contentType(name string) string {
	if t, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
