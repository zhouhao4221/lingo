package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

func TestForwardAttachmentOutcomes(t *testing.T) {
	ok := func(ticket.Attachment, io.Reader) (*gitea.Attachment, error) {
		return &gitea.Attachment{BrowserDownloadURL: "http://g/a/1"}, nil
	}
	drain := func(a ticket.Attachment, r io.Reader) (*gitea.Attachment, error) {
		if _, err := io.Copy(io.Discard, r); err != nil {
			return nil, err
		}
		return ok(a, r)
	}
	content := func(b []byte) func(context.Context, ticket.Attachment) (io.ReadCloser, error) {
		return func(context.Context, ticket.Attachment) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
	}
	failing := func(err error) func(context.Context, ticket.Attachment) (io.ReadCloser, error) {
		return func(context.Context, ticket.Attachment) (io.ReadCloser, error) { return nil, err }
	}
	for name, c := range map[string]struct {
		fetch    func(context.Context, ticket.Attachment) (io.ReadCloser, error)
		upload   func(ticket.Attachment, io.Reader) (*gitea.Attachment, error)
		size     int64
		failures int
		state    ticket.AttachmentState
		retry    bool
	}{
		"no fetch: link":     {nil, ok, 0, 0, ticket.AttachmentLinked, false},
		"uploaded":           {content([]byte("x")), ok, 0, 0, ticket.AttachmentUploaded, false},
		"not owned":          {failing(ticket.ErrNotFound), ok, 0, 0, ticket.AttachmentFailed, false},
		"transient: retry":   {failing(errors.New("503")), ok, 0, 0, ticket.AttachmentPending, true},
		"transient: give up": {failing(errors.New("503")), ok, 0, fetchAttempts - 1, ticket.AttachmentFailed, false},
		"declared too large": {content([]byte("x")), ok, maxAttachmentBytes + 1, 0, ticket.AttachmentFailed, false},
		"gitea attachments disabled": {content([]byte("x")), func(ticket.Attachment, io.Reader) (*gitea.Attachment, error) {
			return nil, &gitea.APIError{StatusCode: 404, Message: "not found"}
		}, 0, 0, ticket.AttachmentFailed, false},
		"actual too large": {func(context.Context, ticket.Attachment) (io.ReadCloser, error) {
			return io.NopCloser(io.LimitReader(zeros{}, maxAttachmentBytes+10)), nil
		}, drain, 0, 0, ticket.AttachmentFailed, false},
	} {
		w := &Worker{o: Options{FetchAttachment: c.fetch}, log: slog.New(slog.DiscardHandler)}
		a := ticket.Attachment{ID: "f1", Name: "a.png", Size: c.size}
		err := w.forwardAttachment(withFailures(context.Background(), c.failures), &a, c.upload)
		if (err != nil) != c.retry || a.State != c.state {
			t.Errorf("%s: state %q err %v", name, a.State, err)
		}
		if c.state == ticket.AttachmentFailed && a.Error == "" {
			t.Errorf("%s: no reason", name)
		}
	}
}

func TestAttachmentSection(t *testing.T) {
	s := attachmentSection([]ticket.Attachment{
		{Name: "a.png", ContentType: "image/png", State: ticket.AttachmentUploaded, GiteaURL: "http://g/1"},
		{Name: "b[1].log", URL: "https://app/f/b", State: ticket.AttachmentLinked},
		{Name: "c.txt", URL: "https://app/f/c", State: ticket.AttachmentFailed},
		{Name: "d.txt", State: ticket.AttachmentLinked},
		{Name: "pending.txt"},
	})
	for _, want := range []string{"![a.png](<http://g/1>)", "[b\\[1\\].log](<https://app/f/b>)", "[c.txt](<https://app/f/c>)（未能上传到 Gitea）", "- d.txt（请在业务系统中查看）"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "pending") || attachmentSection(nil) != "" {
		t.Errorf("section = %s", s)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
