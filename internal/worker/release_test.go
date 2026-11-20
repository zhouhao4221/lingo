package worker

import "testing"

func TestWorkflowIs(t *testing.T) {
	for p, want := range map[string]bool{
		".gitea/workflows/deploy-prod.yml@refs/heads/main": true,
		".gitea/workflows/deploy-prod.yml":                 true,
		".gitea/workflows/deploy-staging.yml@refs/tags/v1": false,
		"": false,
	} {
		if got := workflowIs(p, "deploy-prod.yml"); got != want {
			t.Errorf("workflowIs(%q) = %v", p, got)
		}
	}
}

func TestReleaseRefsAndPrerelease(t *testing.T) {
	refs := releaseRefs("demo/api", "修复 #12，见 demo/web#3 与 `#99`")
	if len(refs) != 2 || refs[0].Repo != "demo/api" || refs[0].Text != "#12" || refs[1].Repo != "demo/web" || refs[1].Text != "demo/web#3" {
		t.Errorf("refs = %+v", refs)
	}
	for tag, want := range map[string]bool{"v1.2.0-rc.1": true, "1.0.0-beta": true, "v1.2.0": false, "release-2026": false} {
		if semverPre.MatchString(tag) != want {
			t.Errorf("prerelease(%s) != %v", tag, want)
		}
	}
	for tag, want := range map[string]bool{"v1.2.0": true, "1.0.0-rc.1": true, "v2.0.0+build.5": true, "backup-2026-11-20": false, "v1.2": false, "deploy-marker": false} {
		if versionTag.MatchString(tag) != want {
			t.Errorf("versionTag(%s) != %v", tag, want)
		}
	}
}
