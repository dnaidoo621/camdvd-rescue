# HTTP API

JSON over HTTP on the app's port. With `CAMDVD_PASSWORD` set, every call
needs the session cookie from `POST /login` (form field `password`); without
it the API is open to the LAN. State-changing requests from a browser must
come from the same origin.

| Method | Path | Body | Returns |
| --- | --- | --- | --- |
| GET | `/api/health` | | `{status, version}`; never needs a login |
| GET | `/api/drives` | | Drives: id, block/sg devices, model, status, holder job, notice |
| POST | `/api/drives/{id}/eject` | | Ejects unless a job is reading (409) |
| POST | `/api/drives/{id}/insert` | `{image}` | Demo drives only: load an image |
| GET | `/api/jobs?state=` | | Discs, newest first, optionally one state |
| GET | `/api/jobs/{id}` | | Disc with sides, stages, evidence and clips |
| POST | `/api/jobs/{id}/answers` | `{sides: 1\|2, description?}` | Updated disc |
| POST | `/api/jobs/{id}/cancel` | | |
| POST | `/api/jobs/{id}/resume` | | |
| POST | `/api/jobs/{id}/force-raw` | | Re-process from the image with raw recovery |
| POST | `/api/jobs/{id}/reprocess` | | Classify and extract a cancelled or failed job again from its saved image |
| POST | `/api/jobs/{id}/import-again` | | Answer "Import again?" with yes |
| POST | `/api/jobs/{id}/add-side` | | Reopen a finished disc for side B |
| DELETE | `/api/jobs/{id}` | | Forget a finished job and its working files (MP4s stay) |
| GET | `/api/library` | | Finished discs with clips, duration, size, earliest date |
| PATCH | `/api/library/folders/{id}` | `{description?, folder?, make?, model?, tz?, disc_date?, location?, imported?}` | Folder; tags rewritten when needed |
| POST | `/api/library/rescan` | | `{relinked, missing, unknown}` |
| POST | `/api/rename/preview` | `{selection: {clips?, folder_files?, folders?, all_files?}, pattern: {template, start, step, find, replace, regex, case}}` | Preview with `id`, `ops` (old/new name, conflict), `clean` |
| POST | `/api/rename/apply` | `{preview_id}` | Batch (409 if the preview has conflicts) |
| POST | `/api/rename/undo/{batchId}` | | Batch marked undone |
| GET | `/api/rename/batches` | | Recent batches |
| GET | `/api/files/{clipId}/stream` | | The clip, with range requests; `?download=1` for an attachment |
| GET | `/api/files/{clipId}/thumb` | | JPEG thumbnail |
| GET | `/api/doctor` | | Self-check report (503 when failing) |
| GET | `/api/events` | | Server-Sent Events: `drives`, `jobs`, `job-<id>`, `library`, `notice` |

Errors are plain text with 400 (bad input), 404 (unknown id) or 409 (busy or
conflicting).

## Example

```sh
H=http://server:8780
curl -s $H/api/jobs?state=awaiting-answer
curl -s -X POST $H/api/jobs/d-7f3a9c/answers -d '{"sides":2,"description":"2004-12 Durban holiday"}'
curl -s -X PATCH $H/api/library/folders/d-7f3a9c -d '{"disc_date":"2004-12-24T10:00","model":"VDR-M50"}'
curl -N $H/api/events
```

## Job states

`detected`, `probing`, `imaging-a`, `awaiting-answer`, `awaiting-flip`,
`imaging-b`, `processing`, `finalizing`, `done`, `failed`, `cancelled`,
`paused`, `duplicate`. See [architecture.md](architecture.md).
