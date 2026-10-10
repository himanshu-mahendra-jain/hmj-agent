package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// After six turns, the turn that follows first asks the model to summarize the turns older
// than the last three, then sends the prompt with that summary in place of them.
func TestLongConversationIsSummarizedByTheModel(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	for i := 1; i <= 7; i++ {
		p.Queue(say(fmt.Sprintf("answer %d", i)))
		s.Type(fmt.Sprintf("question %d\r", i))
		s.Expect(fmt.Sprintf("answer %d", i))
		s.ExpectPrompt()
	}
	if n := len(p.Requests()); n != 7 {
		t.Fatalf("%d requests after 7 turns, want 7: nothing is summarized up to six turns", n)
	}

	// The eighth turn: the summary request is answered first, then the turn itself.
	p.Queue(say("SUMMARY: the user asked seven questions"), say("answer 8"))
	s.Type("question 8\r")
	s.Expect("[condensed 4 earlier turns into a summary")
	s.Expect("answer 8")
	s.ExpectPrompt()

	reqs := p.Requests()
	if len(reqs) != 9 {
		t.Fatalf("%d requests, want 9: seven turns, the summary request and the eighth turn", len(reqs))
	}
	summaryReq, turnReq := reqs[7], reqs[8]
	if !strings.Contains(summaryReq, "Write a summary of the conversation above") || !strings.Contains(summaryReq, `"question 1"`) ||
		!strings.Contains(summaryReq, `"answer 4"`) || strings.Contains(summaryReq, `"question 5"`) || strings.Contains(summaryReq, "question 8") {
		t.Errorf("summary request should carry turns 1-4 and the instruction only:\n%s", summaryReq)
	}
	if !strings.Contains(turnReq, "SUMMARY: the user asked seven questions") || !strings.Contains(turnReq, `"question 5"`) ||
		!strings.Contains(turnReq, `"question 8"`) || strings.Contains(turnReq, `"question 1"`) || strings.Contains(turnReq, "Write a summary") {
		t.Errorf("turn request should carry the summary, turns 5-8 and no summarized turn:\n%s", turnReq)
	}
	s.Quit()

	// The condensed transcript is what is saved and resumed.
	s = e.Start()
	s.Expect("resumed conversation")
	// The conversation is titled after its first prompt, so look for replayed turns only.
	if screen := s.Screen(); strings.Contains(screen, "you: question 1") || !strings.Contains(screen, "SUMMARY: the user asked seven questions") || !strings.Contains(screen, "you: question 8") {
		t.Errorf("resumed transcript should start from the summary")
	}
	s.Quit()
}

// A summary request that fails leaves the conversation as it was: the turn goes ahead with
// the full history and the user is told.
func TestFailedSummaryKeepsHistory(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	for i := 1; i <= 7; i++ {
		s.Type(fmt.Sprintf("question %d\r", i))
		s.Expect("ok")
		s.ExpectPrompt()
	}
	p.Queue(httpError(400, `{"error":{"message":"no summaries here"}}`), say("answer 8"))
	s.Type("question 8\r")
	s.Expect("[could not condense earlier turns:")
	s.Expect("answer 8")
	s.ExpectPrompt()
	reqs := p.Requests()
	if turnReq := reqs[len(reqs)-1]; !strings.Contains(turnReq, `"question 1"`) || !strings.Contains(turnReq, `"question 8"`) {
		t.Errorf("after a failed summary the turn should carry the whole history:\n%s", turnReq)
	}
	s.Quit()
}
