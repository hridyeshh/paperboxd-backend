# Messages (DMs)

Plain 1:1 messaging. What makes it Paperboxd is what you can attach: a book, a list, a thought, a profile, or a Fusion. The object starts the conversation, and the conversation can go anywhere after that.

API reference: `MOBILE_API.md` §3.13. Schema: `migrations/000057_messages.up.sql`. Code: `internal/service/messages.go`, `internal/handler/messages.go`.

## Rules

- **Inbox or Requests.** A new conversation goes to the recipient's inbox if they already follow the sender, and to Requests otherwise. Replying to a request, or tapping Accept, moves it to the inbox.
- **Spam caps.** Until a request is accepted, the sender can send 3 messages. A reader can start 20 new conversations per rolling day. `POST /messages` is limited to 30/min.
- **Blocks.** A block in either direction hides the conversation from both readers and refuses sends. The existing block endpoint handles it; there is no DM-specific block.
- **Attachments.** The server builds each card's `snapshot` at send time; the client never sends one. Other readers' lists and thoughts can only be shared when their account is public, so a card never shows the recipient something the owner hasn't made public. Sending your own private list grants the recipient access, which is the same thing the old list share did. Private thoughts can't be sent. Only a Fusion's two readers can send it.
- **Unsend** blanks the body and deletes the attachments. The row stays behind as a "Message unsent" marker.
- **Delete chat** sets `cleared_at` for the caller only. A new message brings the conversation back, starting from that message.
- **Reports.** `content_type: "message"` copies the body into the report on the server, so unsending after a report doesn't erase the evidence.
- **Account deletion.** Everything cascades when the nightly purge hard-deletes the user. Until then, soft-deleted users are filtered out of every read.

## Delivery

Delivery is polling only. An open chat polls `GET /conversations/{id}/messages?after=<last id>` about every 4s. The header badge polls `GET /conversations/badge` on foreground and about every 30s.

## Not built yet

- **Push.** It waits for Paperboxd Pvt Ltd, the Apple Developer Program and Google Play. Device tokens are already registered (`device_tokens`). Push hooks into `MessageService.Send` after commit: notify the recipient unless they muted the conversation, push only the first message of a request, and collapse notifications per conversation. `muted` already exists and only affects the badge today.
- **Web.** Deliberately app-only for now. The web must not claim DMs exist (site-copy rule).
- **Retire the old share.** `POST /books/{id}/share` (`shared_book` activity) has no callers, and the web version sends the wrong field name. Delete it once the apps' "Send" ships.
- **Privacy policy.** Messages are stored on our servers, are not end-to-end encrypted, and are deleted along with the account. Add this to `docs/PRIVACY_POLICY.md` and the web dialog copy before launch.
- Groups, reactions, typing indicators, edits, media uploads and realtime sockets are all out of scope for v1.
