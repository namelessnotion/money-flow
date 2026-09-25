# Trace fixtures

Hand-written traces that test `spec/Traceeventstore.tla` itself. `spec/trace/validate.sh` expects each `accept-*`
trace to be accepted, and each `reject-*` trace to be rejected by the trace spec rather than by a TLC error. They use
the stream id `x`, and the same header the harness writes (`maxAttempts` 3), unless noted.

| Fixture | What it shows |
|---|---|
| `accept-race` | Two Requests race on one id. The loser's conflict is written *before* the winner's landing, as it can be when lines are written after the database answers. The loser reloads and answers with the winner's decision. |
| `accept-lost-reply` | The winner's INSERT landed but its reply was lost (`Failed`). A retry learns the decision. |
| `accept-error-landed` | An append errored after landing. A later Load shows it landed. |
| `accept-error-not-landed` | An append errored without landing. The next Request decides afresh. |
| `accept-cross-command` | A reversal decided the id first. A transfer on it finds another command's decision and answers `Failed` (`decidedRequestTransfer`'s default case). |
| `accept-aborted` | With `maxAttempts` 1, losing the only race is `Aborted`. |
| `reject-flipped-answer` | The loser is told something other than what the log records. |
| `reject-loser-answers-own-decision` | The loser answers with its own decision instead of reloading. |
| `reject-impossible-load` | A Load sees an event nothing appended. |
| `reject-append-without-load` | An append with no Load to decide against. |
| `reject-undecided` | A successful response with no decision in it. |
| `reject-append-atomic` | A write the design doesn't model. |
