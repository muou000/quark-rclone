# Quark Cookie Session And Operation Contract

Research for [issue #2](https://github.com/muou000/quark-rclone/issues/2), based on the vendored AList `quark_uc` driver in this repository. Line references below are to the current checkout.

## Authentication input and request envelope

- The Quark driver requires one user-supplied `Cookie` string. It also stores a root ID and optional ordering, transcoding, and ranged-download settings; the Quark registration uses root `0` and does not allow overwrite uploads (`alist/drivers/quark_uc/meta.go:8-19`, `29-42`).
- Every normal API request sends the Cookie string, JSON-oriented `Accept`, the Quark referer, and `pr=ucpro` / `fr=pc` query parameters (`alist/drivers/quark_uc/util.go:104-114`). The driver initializes the account with `GET /config`; a non-success response is returned from `Init`, and a successful initialization starts the refresh loop (`alist/drivers/quark_uc/driver.go:59-89`).
- The response envelope is `{status, code, message}`. HTTP transport failures are returned directly; a provider rejection becomes a structured `providerError` carrying HTTP status, provider status, provider code, and message (`alist/drivers/quark_uc/util.go:121-136`, `alist/drivers/quark_uc/types.go:10-30`). There is no generic `IsAuthExpired` helper in this driver, so the client must classify authentication failures from the structured status/code/message and avoid treating every 4xx or 5xx as expired credentials.

## Cookie refresh lifecycle

- Every successful API response is inspected for `Set-Cookie`. `__puus` is merged into the existing Cookie string; `__pus` is also merged when the Quark driver has transcoding enabled. The updated storage is persisted immediately (`alist/drivers/quark_uc/util.go:56-78`). Delete requests use the same refresh behavior (`alist/drivers/quark_uc/delete_reliability.go:124-146`).
- `__puus` is considered short-lived by the driver. A background loop runs every 100 minutes with a random plus/minus five-minute jitter (`alist/drivers/quark_uc/util.go:28-40`, `240-259`). Refresh calls `GET /config` without `__puus`; a failed refresh or a response that does not issue a replacement restores the old Cookie value, preserving concurrent updates where possible (`alist/drivers/quark_uc/util.go:270-296`).
- The refresh loop is cancelled by `Drop`, so the client needs an explicit lifecycle hook when the mount engine stops (`alist/drivers/quark_uc/driver.go:92-100`).

## Read and download contract

- Listing calls `GET /file/sort` page by page with `pdir_fid`, page size 100, total-count fetching, and optional ordering. It returns file IDs, names, sizes and timestamps; HTML entities in names are unescaped (`alist/drivers/quark_uc/util.go:139-177`).
- Download-link creation calls `POST /file/download` with the file ID. The driver snapshots the Cookie before this request because the returned URL signature is tied to that exact Cookie; the returned link must carry the same Cookie, referer, and user agent headers (`alist/drivers/quark_uc/util.go:180-223`). A later response may rotate `__puus`, but that rotated value must not replace the snapshot on the already-created link. The client therefore needs a link/request object that can preserve per-request headers.
- Quark video transcoding is optional and falls back to the normal download link only for the specific `plf_invalid` error; it is not required for ordinary file reads (`alist/drivers/quark_uc/driver.go:106-120`, `alist/drivers/quark_uc/util.go:299-327`).

## Write and management contract

- Create directory uses `POST /file`, then polls the parent listing up to six times with 200 ms initial and 800 ms maximum backoff to account for eventual consistency. A successful create that remains invisible returns no object so higher layers clear stale cache; a same-name conflict is only accepted after the matching directory is observed (`alist/drivers/quark_uc/driver.go:122-201`).
- Move uses `POST /file/move` with the source FID and destination parent FID; rename uses `POST /file/rename` with the FID and new name (`alist/drivers/quark_uc/driver.go:247-268`).
- Copy is explicitly unsupported and returns `errs.NotSupport` (`alist/drivers/quark_uc/driver.go:271-273`).
- Delete uses `POST /file/delete` with the FID. The driver retries bounded transient provider errors, verifies uncertain outcomes through `GET /file/info`, and treats absence as success only for the exact HTTP 404 + provider status 404 + code 21001 signature. Authentication errors, malformed responses, gateway pages, and mismatched FIDs are not absence (`alist/drivers/quark_uc/delete_reliability.go:20-30`, `92-104`, `238-281`, `284-340`).
- Upload is supported as a multipart flow: `POST /file/upload/pre`, hash/update, per-part authorization and OSS upload, multipart commit, then finish. The preflight response supplies a task ID, FID, upload ID, object key, bucket, auth info and part size; the driver validates these before sending data (`alist/drivers/quark_uc/driver.go:301-385`, `alist/drivers/quark_uc/types.go:184-228`, `alist/drivers/quark_uc/upload_reliability.go:132-176`). Control-plane transient failures are retried with context cancellation and bounded exponential backoff; the initial preflight is deliberately not replayed because it may allocate a task/FID (`alist/drivers/quark_uc/upload_reliability.go:77-105`, `162-203`).
- The Quark registration sets `NoOverwriteUpload`, so an overwrite request must be rejected or mapped to an explicit conflict rather than silently replacing an existing file (`alist/drivers/quark_uc/meta.go:29-36`).

## AList WebDAV and proxy dependence

- AList's WebDAV handler maps GET/HEAD to `fs.Get` and `fs.Link`; in native proxy mode it streams through the server with the link headers, while redirect mode returns the URL directly (`alist/server/webdav/webdav.go:216-264`). For a standalone client, the equivalent is a VFS read path that requests a link and forwards its per-link Cookie/referer/user-agent headers.
- WebDAV PUT maps to `fs.PutDirectly`, MKCOL maps to `fs.MakeDir`, and DELETE maps to `fs.Remove` (`alist/server/webdav/webdav.go:303-358`, `361-397`, `268-300`). A direct filesystem adapter can call the corresponding driver operations without implementing WebDAV.
- The AList Quark driver can force `WebProxy`/`WebdavPolicy=native_proxy` for legacy configurations when direct links are not used (`alist/drivers/quark_uc/driver.go:69-85`). The standalone Windows mount should preserve the upstream request headers and proxy/stream the response where the CDN URL alone is insufficient; exposing a bare redirect is unsafe for links whose signature depends on the Cookie snapshot.

## Minimal client contract

The first implementation should therefore persist the user-imported Cookie as encrypted secret material, retain the selected Quark root FID and mount profile separately, merge and persist `__puus` (and optional `__pus`) updates atomically, keep per-download request headers, expose list/read/write/mkdir/move/rename/delete, report copy as unsupported, reject overwrite uploads, and classify provider/auth failures from structured response fields. It should provide a lifecycle stop hook for the refresh loop and avoid assuming that a successful link response means a future request can omit the original Cookie headers.

## Open questions

- The exact provider error codes used for session expiry are not centralized in this driver; live API observations or an integration test matrix are needed before automatic re-authentication rules are finalized.
- The standalone implementation still needs a decision on whether to reuse the AList driver in-process, put it behind a local service, or port its behavior to an rclone backend.
