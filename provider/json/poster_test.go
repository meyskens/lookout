package json

import (
	"bytes"
	"context"
	"testing"

	"gopkg.in/meyskens/lookout-sdk.v0/pb"

	"github.com/meyskens/lookout"

	"github.com/stretchr/testify/require"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

var (
	hash1 = "f67e5455a86d0f2a366f1b980489fac77a373bd0"
	hash2 = "02801e1a27a0a906d59530aeb81f4cd137f2c717"
	base1 = plumbing.ReferenceName("base")
	head1 = plumbing.ReferenceName("refs/pull/42/head")
)

func TestPoster_Post_OK(t *testing.T) {
	require := require.New(t)

	var b bytes.Buffer

	p := NewPoster(&b)
	ev := &lookout.ReviewEvent{
		ReviewEvent: pb.ReviewEvent{
			Provider: Provider,
			CommitRevision: lookout.CommitRevision{
				Base: lookout.ReferencePointer{
					InternalRepositoryURL: "https://github.com/foo/bar",
					ReferenceName:         base1,
					Hash:                  hash1,
				},
				Head: lookout.ReferencePointer{
					InternalRepositoryURL: "https://github.com/foo/bar",
					ReferenceName:         head1,
					Hash:                  hash2,
				}}}}
	cs := []*lookout.Comment{&lookout.Comment{
		Text: "This is a global comment",
	}, &lookout.Comment{
		File: "main.go",
		Text: "This is a file comment",
	}, &lookout.Comment{
		File: "main.go",
		Line: 5,
		Text: "This is a line comment",
	}, &lookout.Comment{
		Text: "This is a another global comment",
	}}

	aCommentsList := []lookout.AnalyzerComments{lookout.AnalyzerComments{
		Config: lookout.AnalyzerConfig{
			Name: "mock",
		},
		Comments: cs,
	}}

	err := p.Post(context.Background(), ev, aCommentsList, false)
	require.NoError(err)

	expected := `{"analyzer-name":"mock","text":"This is a global comment"}
{"analyzer-name":"mock","file":"main.go","text":"This is a file comment"}
{"analyzer-name":"mock","file":"main.go","line":5,"text":"This is a line comment"}
{"analyzer-name":"mock","text":"This is a another global comment"}
`

	require.Equal(expected, b.String())
}
