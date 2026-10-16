package outlookmail

import "github.com/pA1nD/hxmail/internal/hxstore"

// Folder fixed region (class 0x4d).
const (
	fdName = 1088
	fdType = 1160
	// fdWellKnown114 is the server's well-known folder id (7 = Junk Email) in the 16.114 layout,
	// matched against the Exchange GetFolderList ids in the Osa sync log.
	fdWellKnown114 = 1192
	wellKnownJunk  = 7
)

// Folder is a mail folder. Kind is inbox, archive, drafts, sent, deleted, junk, to_me or other.
// Junk Email has the generic folder type 0x7a that user-created folders and several other system
// folders also have; it is recognised by its well-known folder id, which only the 16.114 layout
// is known to carry.
type Folder struct {
	Key    uint32
	Parent uint32
	Name   string
	Kind   string
	// Generic is true for the generic folder type 0x7a. Collect gives such a folder kind to_me
	// when it shares messages with an inbox folder.
	Generic bool
	// BadStrings counts string fields that were present but unreadable and were left blank.
	BadStrings int
}

// typeGeneric is the folder type of user-created folders and of several system folders, Junk
// Email and To Me among them.
const typeGeneric = 0x7a

// KindUnknown is the kind of a message whose folder object the store does not hold.
const KindUnknown = "unknown"

var folderKinds = map[uint32]string{
	0x61: "inbox", 0x63: "archive", 0x64: "drafts", 0x65: "sent", 0x67: "deleted",
}

// MapFolder maps a class 0x4d object. The generic type 0x7a and an unknown well-known type are
// kind other. The type values are likely, not established, outside the English folder names they
// were measured on.
func MapFolder(o hxstore.Object) (Folder, error) {
	fixed, shift := TagFolder, 0
	if o.Tag == TagFolder114 {
		fixed, shift = TagFolder114, 8
	}
	f, err := newFields(o, fixed)
	if err != nil {
		return Folder{}, err
	}
	fd := Folder{Key: word(o, offKey), Parent: word(o, offLink), Name: f.text(fdName + shift), Kind: "other"}
	fd.BadStrings = f.bad
	fd.Generic = word(o, fdType+shift) == typeGeneric
	if k, ok := folderKinds[word(o, fdType+shift)]; ok {
		fd.Kind = k
	}
	if shift == 8 && word(o, fdWellKnown114) == wellKnownJunk {
		fd.Kind, fd.Generic = "junk", false
	}
	return fd, nil
}
