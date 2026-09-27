package service

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSend(t *testing.T) {
	book := []AttachmentRef{{Kind: AttachBook, RefID: "b1"}}
	cases := []struct {
		name string
		in   SendInput
		ok   bool
	}{
		{"text to a conversation", SendInput{ConversationID: "c", Body: "hi"}, true},
		{"attachment only", SendInput{To: []string{"maya"}, Attachments: book}, true},
		{"empty", SendInput{ConversationID: "c", Body: "   "}, false},
		{"no target", SendInput{Body: "hi"}, false},
		{"too long", SendInput{ConversationID: "c", Body: strings.Repeat("a", MaxMessageBody+1)}, false},
		{"max length in runes", SendInput{ConversationID: "c", Body: strings.Repeat("é", MaxMessageBody)}, true},
		{"too many recipients", SendInput{To: []string{"a", "b", "c", "d", "e", "f"}, Body: "hi"}, false},
		{"bad kind", SendInput{ConversationID: "c", Attachments: []AttachmentRef{{Kind: "photo", RefID: "x"}}}, false},
		{"blank ref", SendInput{ConversationID: "c", Attachments: []AttachmentRef{{Kind: AttachBook, RefID: " "}}}, false},
		{"too many attachments", SendInput{ConversationID: "c", Attachments: make([]AttachmentRef, MaxMessageAttachments+1)}, false},
		{"bad client id", SendInput{ConversationID: "c", Body: "hi", ClientID: "nope"}, false},
		{"good client id", SendInput{ConversationID: "c", Body: "hi", ClientID: "7d9f1c1e-2b1a-4c1e-9f1a-1b2c3d4e5f60"}, true},
	}
	for _, c := range cases {
		err := validateSend(c.in)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: want ErrInvalidMessage, got %v", c.name, err)
		}
	}
}

func TestInitialState(t *testing.T) {
	if initialState(true) != ConvAccepted {
		t.Error("a reader who follows the sender should get the message in their inbox")
	}
	if initialState(false) != ConvRequest {
		t.Error("a reader who does not follow the sender should get a request")
	}
}

func TestPreviewText(t *testing.T) {
	cases := []struct {
		body, kind string
		unsent     bool
		want       string
	}{
		{"This is  SO\nyou", AttachBook, false, "This is SO you"},
		{"", AttachList, false, "Sent a list"},
		{"", AttachFusion, false, "Sent a Fusion"},
		{"hi", AttachBook, true, "Message unsent"},
		{"", "", false, ""},
	}
	for _, c := range cases {
		if got := previewText(c.body, c.kind, c.unsent); got != c.want {
			t.Errorf("previewText(%q, %q, %v) = %q, want %q", c.body, c.kind, c.unsent, got, c.want)
		}
	}
	long := previewText(strings.Repeat("あ", 200), "", false)
	if n := len([]rune(long)); n != 120 || !strings.HasSuffix(long, "…") {
		t.Errorf("long preview should be cut to 120 runes with an ellipsis, got %d runes", n)
	}
}
