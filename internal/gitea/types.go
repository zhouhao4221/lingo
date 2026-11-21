package gitea

import "time"

// 以下类型对照 Gitea v1.27.3 swagger，只保留灵工用到的字段。

// User 是 Gitea 用户。
type User struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
	IsAdmin  bool   `json:"is_admin"`
	HTMLURL  string `json:"html_url"`
}

// InternalTracker 是仓库内置 Issue 跟踪的设置。
type InternalTracker struct {
	EnableTimeTracker                bool `json:"enable_time_tracker"`
	AllowOnlyContributorsToTrackTime bool `json:"allow_only_contributors_to_track_time"`
	EnableIssueDependencies          bool `json:"enable_issue_dependencies"`
}

// ExternalTracker 是外部 Issue 跟踪设置；非空表示仓库把 Issue 交给了外部系统。
type ExternalTracker struct {
	URL    string `json:"external_tracker_url"`
	Format string `json:"external_tracker_format"`
	Style  string `json:"external_tracker_style"`
}

// Permission 是当前令牌对仓库的权限。
type Permission struct {
	Admin bool `json:"admin"`
	Push  bool `json:"push"`
	Pull  bool `json:"pull"`
}

// Repository 是 Gitea 仓库。
type Repository struct {
	ID              int64            `json:"id"`
	Owner           *User            `json:"owner"`
	Name            string           `json:"name"`
	FullName        string           `json:"full_name"`
	HTMLURL         string           `json:"html_url"`
	DefaultBranch   string           `json:"default_branch"`
	Archived        bool             `json:"archived"`
	Empty           bool             `json:"empty"`
	HasIssues       bool             `json:"has_issues"`
	HasPullRequests bool             `json:"has_pull_requests"`
	HasReleases     bool             `json:"has_releases"`
	HasActions      bool             `json:"has_actions"`
	HasWiki         bool             `json:"has_wiki"`
	InternalTracker *InternalTracker `json:"internal_tracker"`
	ExternalTracker *ExternalTracker `json:"external_tracker"`
	Permissions     *Permission      `json:"permissions"`
}

// Label 是标签；名称含 "/" 且 Exclusive 为真时为作用域标签，同一作用域互斥。
type Label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Exclusive   bool   `json:"exclusive"`
	IsArchived  bool   `json:"is_archived"`
}

// Milestone 是里程碑。
type Milestone struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"`
	DueOn       *time.Time `json:"due_on"`
}

// Attachment 是 Issue、评论或 Release 的附件。
type Attachment struct {
	ID                 int64     `json:"id"`
	UUID               string    `json:"uuid"`
	Name               string    `json:"name"`
	Size               int64     `json:"size"`
	BrowserDownloadURL string    `json:"browser_download_url"`
	CreatedAt          time.Time `json:"created_at"`
}

// PullRequestMeta 出现在 Issue 上，非空表示该 Issue 实为 PR。
type PullRequestMeta struct {
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	HTMLURL  string     `json:"html_url"`
}

// RepositoryMeta 是 Issue 所在仓库的简要信息。
type RepositoryMeta struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	FullName string `json:"full_name"`
}

// Issue 状态。
const (
	StateOpen   = "open"
	StateClosed = "closed"
	StateAll    = "all"
)

// Issue 是 Gitea Issue。
type Issue struct {
	ID          int64            `json:"id"`
	Number      int64            `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	State       string           `json:"state"`
	User        *User            `json:"user"`
	Labels      []*Label         `json:"labels"`
	Milestone   *Milestone       `json:"milestone"`
	Assignees   []*User          `json:"assignees"`
	IsLocked    bool             `json:"is_locked"`
	Comments    int              `json:"comments"`
	Ref         string           `json:"ref"`
	HTMLURL     string           `json:"html_url"`
	DueDate     *time.Time       `json:"due_date"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	ClosedAt    *time.Time       `json:"closed_at"`
	PullRequest *PullRequestMeta `json:"pull_request"`
	Repository  *RepositoryMeta  `json:"repository"`
	Assets      []*Attachment    `json:"assets"`
}

// Comment 是 Issue 评论。
type Comment struct {
	ID        int64         `json:"id"`
	Body      string        `json:"body"`
	User      *User         `json:"user"`
	HTMLURL   string        `json:"html_url"`
	IssueURL  string        `json:"issue_url"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Assets    []*Attachment `json:"assets"`
}

// Timeline 事件类型（TimelineComment.Type）中灵工关心的取值。
const (
	TimelineComment    = "comment"
	TimelineClose      = "close"
	TimelineReopen     = "reopen"
	TimelineCommitRef  = "commit_ref" // 被提交引用，见 RefCommitSHA
	TimelinePullRef    = "pull_ref"   // 被 PR 引用，见 RefIssue
	TimelineIssueRef   = "issue_ref"
	TimelineLabel      = "label"
	TimelineMilestone  = "milestone"
	TimelineAssignees  = "assignees"
	TimelineDependency = "add_dependency"
)

// TimelineEvent 是 Issue timeline 中的一条记录（评论与各类事件）。
type TimelineEvent struct {
	ID           int64      `json:"id"`
	Type         string     `json:"type"`
	Body         string     `json:"body"`
	User         *User      `json:"user"`
	Label        *Label     `json:"label"`
	Milestone    *Milestone `json:"milestone"`
	Assignee     *User      `json:"assignee"`
	RefIssue     *Issue     `json:"ref_issue"`
	RefCommitSHA string     `json:"ref_commit_sha"`
	RefAction    string     `json:"ref_action"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// PRBranchInfo 是 PR 的源 / 目标分支。
type PRBranchInfo struct {
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	RepoID int64  `json:"repo_id"`
}

// PullRequest 是合并请求。
type PullRequest struct {
	ID             int64         `json:"id"`
	Number         int64         `json:"number"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	State          string        `json:"state"`
	User           *User         `json:"user"`
	HTMLURL        string        `json:"html_url"`
	Merged         bool          `json:"merged"`
	MergedAt       *time.Time    `json:"merged_at"`
	MergeCommitSHA string        `json:"merge_commit_sha"`
	Base           *PRBranchInfo `json:"base"`
	Head           *PRBranchInfo `json:"head"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// CommitMeta 是提交的简要引用。
type CommitMeta struct {
	SHA     string     `json:"sha"`
	URL     string     `json:"url"`
	Created *time.Time `json:"created"`
}

// CommitUser 是提交作者 / 提交者。
type CommitUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

// RepoCommit 是提交内容。
type RepoCommit struct {
	Message   string      `json:"message"`
	Author    *CommitUser `json:"author"`
	Committer *CommitUser `json:"committer"`
}

// Commit 是一次提交。
type Commit struct {
	SHA     string        `json:"sha"`
	HTMLURL string        `json:"html_url"`
	Commit  *RepoCommit   `json:"commit"`
	Parents []*CommitMeta `json:"parents"`
}

// Compare 是两个引用之间的提交差异。
type Compare struct {
	TotalCommits int       `json:"total_commits"`
	Commits      []*Commit `json:"commits"`
}

// Release 是版本发布。
type Release struct {
	ID              int64         `json:"id"`
	TagName         string        `json:"tag_name"`
	TargetCommitish string        `json:"target_commitish"`
	Name            string        `json:"name"`
	Body            string        `json:"body"`
	Draft           bool          `json:"draft"`
	Prerelease      bool          `json:"prerelease"`
	HTMLURL         string        `json:"html_url"`
	Author          *User         `json:"author"`
	CreatedAt       time.Time     `json:"created_at"`
	PublishedAt     *time.Time    `json:"published_at"`
	Assets          []*Attachment `json:"assets"`
}

// Tag 是 Git 标签。
type Tag struct {
	Name    string      `json:"name"`
	Message string      `json:"message"`
	ID      string      `json:"id"`
	Commit  *CommitMeta `json:"commit"`
}

// Hook 是仓库或组织的 Webhook。
type Hook struct {
	ID                  int64             `json:"id"`
	Type                string            `json:"type"`
	Name                string            `json:"name"`
	Active              bool              `json:"active"`
	Events              []string          `json:"events"`
	Config              map[string]string `json:"config"`
	BranchFilter        string            `json:"branch_filter"`
	AuthorizationHeader string            `json:"authorization_header"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// URL 返回 Webhook 的投递地址。
func (h *Hook) URL() string { return h.Config["url"] }

// Reaction 是对 Issue / 评论的表情回应。
type Reaction struct {
	Content   string    `json:"content"`
	User      *User     `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// CommitStatus 是提交状态。
type CommitStatus struct {
	ID          int64     `json:"id"`
	Context     string    `json:"context"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
	TargetURL   string    `json:"target_url"`
	CreatedAt   time.Time `json:"created_at"`
}

// Workflow 是 Gitea Actions 工作流定义（需 Gitea ≥ 1.25）。
type Workflow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// WorkflowRun 是工作流的一次运行（需 Gitea ≥ 1.25）。
type WorkflowRun struct {
	ID           int64      `json:"id"`
	DisplayTitle string     `json:"display_title"`
	Path         string     `json:"path"` // 工作流文件路径，如 ".gitea/workflows/deploy-prod.yml@refs/heads/main"
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HeadBranch   string     `json:"head_branch"`
	HeadSHA      string     `json:"head_sha"`
	RunNumber    int64      `json:"run_number"`
	RunAttempt   int64      `json:"run_attempt"`
	HTMLURL      string     `json:"html_url"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
}

// ServerVersion 是实例版本。
type ServerVersion struct {
	Version string `json:"version"`
}

// APISettings 是实例的 API 设置。
type APISettings struct {
	MaxResponseItems       int   `json:"max_response_items"`
	DefaultPagingNum       int   `json:"default_paging_num"`
	DefaultMaxBlobSize     int64 `json:"default_max_blob_size"`
	DefaultMaxResponseSize int64 `json:"default_max_response_size"`
}

// AttachmentSettings 是实例的附件设置。
type AttachmentSettings struct {
	Enabled      bool   `json:"enabled"`
	AllowedTypes string `json:"allowed_types"` // 逗号分隔的扩展名或 MIME 类型
	MaxSize      int64  `json:"max_size"`      // 单个附件上限，单位 MB
	MaxFiles     int    `json:"max_files"`
}

// CollaboratorPermission 是某用户对仓库的权限。
type CollaboratorPermission struct {
	Permission string `json:"permission"` // none | read | write | admin | owner
	RoleName   string `json:"role_name"`
	User       *User  `json:"user"`
}

// CanWrite 报告权限是否至少为 write。
func (p *CollaboratorPermission) CanWrite() bool {
	switch p.Permission {
	case "write", "admin", "owner":
		return true
	}
	return false
}

// IssueMeta 定位一个 Issue（可跨仓库）。
type IssueMeta struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Index int64  `json:"index"`
}
