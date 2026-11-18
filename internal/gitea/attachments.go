package gitea

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
)

// CreateIssueAttachment 上传 Issue 附件（multipart 字段 attachment），name 为显示文件名。
// r 以流式写入请求体，不整体读入内存。
func (c *Client) CreateIssueAttachment(ctx context.Context, owner, repo string, index int64, name string, r io.Reader) (*Attachment, error) {
	return c.upload(ctx, issuePath(owner, repo, index)+"/assets", name, r)
}

// CreateCommentAttachment 上传评论附件。
func (c *Client) CreateCommentAttachment(ctx context.Context, owner, repo string, commentID int64, name string, r io.Reader) (*Attachment, error) {
	return c.upload(ctx, commentPath(owner, repo, commentID)+"/assets", name, r)
}

func (c *Client) upload(ctx context.Context, path, name string, r io.Reader) (*Attachment, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("attachment", name)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	req, err := c.newRequest(ctx, "POST", path, q, pr, mw.FormDataContentType())
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	var a Attachment
	if _, err := c.send(req, &a); err != nil {
		pr.CloseWithError(err)
		return nil, fmt.Errorf("upload %s: %w", name, err)
	}
	return &a, nil
}
