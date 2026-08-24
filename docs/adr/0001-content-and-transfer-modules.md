# Concentrate content mutations and file transfers

Content identity, revisions, metadata, favorites and ordered events belong to one content-lifecycle module, while upload, URL download, temporary files and crash recovery belong to one file-transfer module. HTTP remains an adapter at those seams; this preserves the existing REST and SSE interface while preventing lifecycle invariants from leaking back into route handlers.

File publication uses a recoverable commit record: failures before the record is durable clean their payload, while later failures remain invisible until restart recovery completes metadata and the atomic publish. This was chosen over independent best-effort writes because the latter can expose files without identity, metadata or a matching event.
