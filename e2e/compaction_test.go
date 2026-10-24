package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// overLimit is a prompt size over the 400K tokens at which a conversation is compacted.
const overLimit = 450_000

// Turns are never compacted by count: a conversation stays whole until it is over the limit.
// Then, once the turn has ended, it is compacted with the provider's compact endpoint while the
// next prompt is typed, and the next turn starts from the compacted window.
func TestConversationIsCompactedOnlyOverTheLimit(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	e, s := start(t, p)
	for i := 1; i <= 8; i++ {
		p.Queue(say(fmt.Sprintf("answer %d", i)))
		s.Type(fmt.Sprintf("question %d\r", i))
		s.Expect(fmt.Sprintf("answer %d", i))
		s.ExpectPrompt()
	}
	if n := len(p.Requests()); n != 8 {
		t.Fatalf("%d requests after 8 small turns, want 8: nothing is compacted under the limit", n)
	}

	// The ninth turn reports a prompt over the limit; the compact request follows it, and the
	// next prompt is sent once it is in place.
	p.Queue(sayWithPrompt("answer 9", overLimit), say("answer 10"))
	p.QueueCompact(compactedWindow("WINDOW-OF-NINE-TURNS"))
	s.Type("question 9\r")
	s.Expect("answer 9")
	s.ExpectPrompt()
	s.Type("question 10\r")
	s.Expect("[compacted the conversation: ~450,")
	s.Expect("answer 10")
	s.ExpectPrompt()

	reqs := p.Requests()
	if len(reqs) != 11 {
		t.Fatalf("%d requests, want 11: nine turns, the compact request and the tenth turn", len(reqs))
	}
	if compactReq := reqs[9]; !strings.Contains(compactReq, `"question 1"`) || !strings.Contains(compactReq, `"answer 9"`) {
		t.Errorf("compact request should carry the whole conversation:\n%s", compactReq)
	}

	if turnReq := p.LastRequest(); !strings.Contains(turnReq, "WINDOW-OF-NINE-TURNS") ||
		!strings.Contains(turnReq, `"question 10"`) || strings.Contains(turnReq, `"question 9"`) {
		t.Errorf("the next turn should carry the compacted window and the new prompt only:\n%s", turnReq)
	}
	s.Quit()

	// The compacted transcript is what is saved and resumed.
	s = e.Start()
	s.Expect("resumed conversation: Mock: question 1 ")
	if screen := s.Screen(); strings.Contains(screen, "you: question 9") || !strings.Contains(screen, "[the earlier conversation was compacted]") ||
		!strings.Contains(screen, "you: question 10") {
		t.Errorf("resumed transcript should start from the compaction:\n%s", screen)
	}
	p.Queue(say("answer 11"))
	s.Type("question 11\r")
	s.Expect("answer 11")
	if turnReq := p.LastRequest(); !strings.Contains(turnReq, "WINDOW-OF-NINE-TURNS") || !strings.Contains(turnReq, `"question 10"`) {
		t.Errorf("after a restart the turn should still carry the compacted window:\n%s", turnReq)
	}
	s.ExpectPrompt()

	// /history still lists and shows the compacted turns, before the turns since.
	s.Type("\x19") // Ctrl+Y
	s.Expect("browse turns")
	s.TypeSlowly("question 9")
	s.Expect("[9/11] (compacted) question 9")
	s.Type("\r")
	s.Expect("choose action")
	s.TypeSlowly("view")
	s.Type("\r")
	s.Expect("answer 9")
	s.ExpectPrompt()
	s.Quit()
}

// A compact request that fails leaves the conversation as it was: the user is told, the next turn
// carries the whole history, and compaction is tried again after it.
func TestFailedCompactionKeepsHistory(t *testing.T) {
	t.Parallel()
	p := newProvider(t)
	_, s := start(t, p)
	p.Queue(sayWithPrompt("answer 1", overLimit), sayWithPrompt("answer 2", overLimit), say("answer 3"))
	p.QueueCompact(httpError(401, `{"error":{"message":"no compaction here"}}`))
	s.Type("question 1\r")
	s.Expect("answer 1")
	s.ExpectPrompt()

	s.Type("question 2\r")
	s.Expect("[could not compact the conversation:")
	s.Expect("answer 2")
	s.ExpectPrompt()
	if turnReq := p.LastRequest(); !strings.Contains(turnReq, `"question 1"`) || !strings.Contains(turnReq, `"question 2"`) {
		t.Errorf("after a failed compaction the turn should carry the whole history:\n%s", turnReq)
	}

	s.Type("question 3\r")
	s.Expect("[compacted the conversation:")
	s.Expect("answer 3")
	s.ExpectPrompt()
	if turnReq := p.LastRequest(); strings.Contains(turnReq, `"question 1"`) || !strings.Contains(turnReq, `"question 3"`) {
		t.Errorf("the turn after a compaction should carry the window and the new prompt only:\n%s", turnReq)
	}
	s.Quit()
}
