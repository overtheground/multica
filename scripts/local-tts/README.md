Local Irina replies (MUL-49)
===========================

The existing `/home/desktop/local-tts/venv` supplies Piper, ONNX Runtime,
NumPy and PyAV; `requirements.lock` in that directory records installed
versions. No new model, service, timer, paid API or background process is needed.
`reply.py` uses the installed `ru_RU-irina-medium.onnx`, a +2 semitone
`asetrate -> aresample -> atempo` chain, 0.75 gain, mono Opus at 48 kHz / 32 kbps.
It preserves all input characters across 1800-character chunks. Each invocation
creates a private fresh directory; it never reads previous replies.

Install the helper by copying `reply.py` to `/home/desktop/local-tts/reply.py`.
The durable Mika instruction saved for MUL-49 invokes it only for new private
Telegram replies to Max, including composed task-status replies:

```bash
timeout 180s /home/desktop/local-tts/venv/bin/python /home/desktop/local-tts/reply.py \
  --text-file ./voice-reply.txt --output-dir ./voice-output --upload
```

Save the final text unchanged first; return that same text even if the helper
fails or times out. Omit `--upload` for local synthesis/codec checks. Uploads use
only `multica attachment upload` for the current task, once per part, with no
blind retries. A successful upload is NOT evidence of Telegram delivery.
Synthesis completes for all chunks before any upload starts; an upload failure
can still leave earlier parts accepted. The text reply must always survive.

This is an agent-instruction workflow, not a deterministic daemon completion
hook. Future agents must follow the saved instruction; it is scoped to the
identified recipient and channel, not every Mika task. The durable preference
record is `/home/desktop/local-tts/voice-preference.json`. Issue/comment replies
and old history are excluded. Voice output files remain in the task's output
directory; no automatic history scan, replay, or cleanup timer is installed.

Backend activation is separate. The deployed MUL-32 notification worker does
not send files. MUL-49 wires the same attachment transport used by ordinary
replies into the notification worker, after its canonical text is settled.
`sent` on the notification outbox means text delivered; per-file Bot API success
or failure is logged separately. On process loss during the media hop, remaining
files can still be lost; uncertain sends are deliberately not retried because
Telegram has no idempotency key. This change fixes the missing event path; it
does not introduce a persistent per-file delivery outbox.

PR #2 (`mul-46-piper-voice-replies`) is independent: it chooses `sendVoice` for
named `.voice.ogg` files. This change does not copy or require that patch for
attachment transport. The member reports that existing `.voice.ogg` renders as
voice already; verify the real presentation during the one activation probe.

Verification:

```bash
python3 -m unittest discover -s scripts/local-tts -p 'test_*.py'
# Against a migrated, isolated test DB only:
cd server
go test ./internal/handler -run TestTelegramReplyAttachmentsByTrigger -count=1 -v
go test ./internal/integrations/telegram -count=1
```

Activation preview: review this diff; build the backend from the reviewed
commit based on deployed `8f2a6ba1`; back up the current backend/config and
record image rollback; replace/restart only the backend with unchanged env,
DB, web, daemon and model files. There are no schema migrations in this diff.
This issue does not authorize deploy/restart. After authorization, use one
appropriate composed status-event reply containing real text + its Irina audio
and a small file, then obtain Telegram-side confirmation. The ordinary reply
path also needs recipient confirmation. Never replay old pending/sent events
or infer delivery from upload IDs.
