package toolshare_test

import (
	"reflect"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

func TestM96_TopicsPinned(t *testing.T) {
	if toolshare.TopicToolShareProposed != "toolshare.tool_share_proposed" {
		t.Errorf("TopicToolShareProposed=%q", toolshare.TopicToolShareProposed)
	}
	if toolshare.TopicToolSharePROpened != "toolshare.tool_share_pr_opened" {
		t.Errorf("TopicToolSharePROpened=%q", toolshare.TopicToolSharePROpened)
	}
}

func TestM96_TargetSource_Validate(t *testing.T) {
	for _, c := range []struct {
		v  toolshare.TargetSource
		ok bool
	}{
		{toolshare.TargetSourcePlatform, true},
		{toolshare.TargetSourcePrivate, true},
		{"", false},
		{"hosted", false},
		{"OTHER", false},
	} {
		err := c.v.Validate()
		if c.ok && err != nil {
			t.Errorf("%q err=%v want nil", c.v, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q err=nil want non-nil", c.v)
		}
	}
}

func TestM96_ToolShareProposed_FieldAllowlist(t *testing.T) {
	wants := []struct{ name, gType string }{
		{"SourceName", "string"},
		{"ToolName", "string"},
		{"ToolVersion", "string"},
		{"ProposerID", "string"},
		{"Reason", "string"},
		{"TargetOwner", "string"},
		{"TargetRepo", "string"},
		{"TargetBase", "string"},
		{"TargetSource", "toolshare.TargetSource"},
		{"ProposedAt", "time.Time"},
		{"CorrelationID", "string"},
	}
	rt := reflect.TypeOf(toolshare.ToolShareProposed{})
	if rt.NumField() != len(wants) {
		t.Fatalf("ToolShareProposed fields=%d want %d", rt.NumField(), len(wants))
	}
	for i, w := range wants {
		f := rt.Field(i)
		if f.Name != w.name {
			t.Errorf("field[%d] name=%q want %q", i, f.Name, w.name)
		}
		if f.Type.String() != w.gType {
			t.Errorf("field[%d] type=%q want %q", i, f.Type.String(), w.gType)
		}
	}
}

func TestM96_ToolSharePROpened_FieldAllowlist(t *testing.T) {
	wants := []struct{ name, gType string }{
		{"SourceName", "string"},
		{"ToolName", "string"},
		{"ToolVersion", "string"},
		{"ProposerID", "string"},
		{"TargetOwner", "string"},
		{"TargetRepo", "string"},
		{"TargetBase", "string"},
		{"TargetSource", "toolshare.TargetSource"},
		{"PRNumber", "int"},
		{"PRHTMLURL", "string"},
		{"OpenedAt", "time.Time"},
		{"CorrelationID", "string"},
	}
	rt := reflect.TypeOf(toolshare.ToolSharePROpened{})
	if rt.NumField() != len(wants) {
		t.Fatalf("ToolSharePROpened fields=%d want %d", rt.NumField(), len(wants))
	}
	for i, w := range wants {
		f := rt.Field(i)
		if f.Name != w.name {
			t.Errorf("field[%d] name=%q want %q", i, f.Name, w.name)
		}
		if f.Type.String() != w.gType {
			t.Errorf("field[%d] type=%q want %q", i, f.Type.String(), w.gType)
		}
	}
}

func TestM96_NoBannedFields(t *testing.T) {
	banned := []string{
		"DataDir", "LivePath", "FolderContent", "ManifestBody",
		"SourceContent", "Token", "AuthSecret", "Credentials",
		"GitHubToken", "SlackChannelID",
	}
	for _, rt := range []reflect.Type{
		reflect.TypeOf(toolshare.ToolShareProposed{}),
		reflect.TypeOf(toolshare.ToolSharePROpened{}),
	} {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			for _, b := range banned {
				if f.Name == b {
					t.Errorf("%s contains banned field %q", rt.Name(), b)
				}
			}
		}
	}
}
