# Adept Messaging Subsystem – Technical Specification

## Introduction

The Adept Messaging subsystem provides asynchronous outbound communication across multiple channels (email, SMS, push notifications, and webhooks) in a **multi-tenant** environment. It is a distributed component of the Adept monolith, running on every instance (20+ nodes) to ensure high availability and throughput. Outbound messages are queued and processed reliably in the background, decoupling send operations from request/trigger flows. This subsystem is designed with Adept’s standard architectural patterns – such as a global queue and provider-agnostic adapters – to align with existing systems (mirroring the Social Share job queue approach for distributed processing). Today, MariaDB is the permanent default database, and the initial queue implementation will target MariaDB. CockroachDB support remains a planned cloud deployment option, and the schema and driver wiring described below will be adapted when that work begins. As outlined in Adept’s roadmap, Messaging Phase 1 introduces a queue table, a worker pool on each node, SendGrid (email) and Twilio (SMS) integrations, per-tenant opt-out lists, and metrics for observability. Phase 2 will extend this to push notifications (FCM/APNS), template management, and provider webhooks for delivery receipts. The following specification details the architecture, data model, processing logic, adapter design, and considerations for extensibility, observability, and security of the messaging subsystem.

## System Architecture and Distributed Operation

Each Adept server instance includes a **message queue worker** that continuously pulls from a global job queue stored in the primary database. The job queue is a **global table** (shared across all tenants and nodes) that holds outbound message tasks awaiting processing. Using transactional consistency, the system achieves **distributed coordination** via row-level locking – ensuring that a given message job is claimed and processed by only one node. This design prevents duplicate sends while allowing any node to handle any tenant’s messages. It mirrors the strategy used by Adept’s Social Share subsystem, where a central table and row locks coordinate work among nodes (providing an effectively “exactly-once” processing guarantee under normal operation). When CockroachDB support is added, the same pattern applies, but with CockroachDB-specific locking semantics and schema adjustments.

On startup, each instance creates a pool of worker goroutines (size configurable, e.g. based on CPU cores or throughput needs) to handle message jobs. Workers periodically poll the global queue for new or retriable jobs. When a worker finds a **pending** job, it will claim it using an atomic transaction that marks the row as in-progress (e.g. via `SELECT … FOR UPDATE` or an equivalent). This lock prevents other nodes from taking the same job. The worker then processes the job outside the transaction (sending the email/SMS/etc), and afterward completes the job by updating or deleting the queue record in a follow-up transaction. By leveraging transactional consistency, this approach simplifies coordination without needing an external queue system – the database serves as the single source of truth for pending work.

**Concurrency & Fault Tolerance:** Multiple nodes and workers can process different jobs in parallel. In case a node crashes or a worker process terminates mid-job, the lock on the job will eventually expire or be released (transaction abort or via a heartbeat timeout strategy), allowing another node to retry the message. Each job record can include a timestamp (e.g. `claimed_at` and an owning node identifier) so that a watchdog process can detect stuck jobs – if a job has been in progress without completion beyond a threshold, it can be considered failed and made available for retry by resetting its status. This mechanism ensures that no message gets lost if a node fails mid-send. The design assumes that sends are idempotent or that providers handle duplicate suppression, in the rare case a retry happens after an unknown partial success.

All Adept application nodes run this subsystem, meaning the messaging service scales horizontally with the monolith. There is no single point of failure – if any node is down, others continue processing the global queue. This design choice (as opposed to a dedicated message broker) keeps infrastructure simple and leverages the existing primary database. Adept’s architecture already uses a global database for coordination (e.g. the tenant registry), and the messaging queue follows suit for consistency. CockroachDB support will re-use this model when the cloud deployment mode is introduced.

## Database Schema

### Global Message Queue Table (MariaDB today, CockroachDB later)

The global **message queue** table (e.g. `message_queue`) resides in the **global database** and holds all pending or in-progress outbound message jobs across all tenants. This table is the heart of the distribution mechanism – it is accessible by every node and supports transactions for locking. Key schema fields include:

* **job\_id** (UUID or BIGINT serial): Primary key identifying the job. Using a UUID can simplify multi-node unique generation.
* **tenant\_id** (BIGINT): Reference to the tenant that generated the message. This links to the global `site` table’s ID (or tenant identifier) to resolve which tenant’s context/credentials to use.
* **channel** (ENUM or VARCHAR): The type of message channel – e.g. `'Email'`, `'SMS'`, `'Push'`, `'Webhook'` (with future extension for `'Voice'`, `'OTT'`, etc.). This field dictates which provider adapter and content fields apply.
* **status** (SMALLINT or ENUM): Current state of the job in the queue, such as `Pending` (queued), `Processing` (claimed by a worker), `Succeeded`, `Failed`. In practice, jobs with `Succeeded` or final `Failed` status may be immediately removed from this table to keep the queue small (alternatively, status could be tracked only in the history table and the queue table pruned on completion).
* **attempt\_count** (INT): How many attempts have been made to send this message. Initialized to 0, and incremented on each send attempt (including the initial send).
* **max\_attempts** (INT): The maximum attempts allowed before giving up (could be a system-wide constant like 5, but storing per job allows override for certain messages or channels).
* **next\_attempt\_at** (TIMESTAMPTZ): Timestamp when the next attempt should be made. For new jobs, this is the enqueue time (immediate). After a failure, this is updated to the scheduled retry time based on exponential backoff. Workers querying for jobs will filter for jobs where `next_attempt_at <= now()` to find ready tasks.
* **priority** (INT, optional): A priority or ordering key if needed (e.g. lower number = higher priority). By default, the queue is FIFO (order by enqueue time), but this field can allow urgent messages to jump the line if the product requires.
* **to\_address** (TEXT): The primary recipient address or target. For Email, this is the email address; for SMS, the phone number; for Push, a device token or user identifier; for Webhook, the endpoint URL.
* **subject** (TEXT, nullable): Subject or title of the message. Used for Email subject line, or Push notification title. (Not used for SMS or webhooks, but the field is kept generic for multi-channel use; it can be null or empty for those channels.)
* **body\_text** (TEXT, nullable): The message body in text form. For emails, this holds the plaintext version of the email. For SMS, this holds the text of the SMS. For Push notifications, this could be the notification body text. For webhooks, this could contain a JSON payload or form data to POST. (In some cases, a `body_html` might be stored separately for email HTML content – see below.)
* **body\_html** (TEXT, nullable): (Email-specific) The HTML content of the email, if any. This allows rich email content to be stored. Not used for SMS or push (could be null for those).
* **attachments** (JSON or TEXT, nullable): Any file attachments or URLs to include. This could be a JSON array of objects with fields like filename and a reference (path or URL). For email, attachments will be read and sent with the email. For other channels, this might be unused or could contain media URLs for MMS or data for push notifications. (E.g., for an MMS via Twilio, one could include a media URL here.)
* **created\_at** (TIMESTAMPTZ): Timestamp when the job was enqueued.
* **updated\_at** (TIMESTAMPTZ): Timestamp when the job record was last updated (e.g. on attempt or status change).

**Indexes:** The table should be indexed to efficiently retrieve pending jobs. A typical access pattern is to find the oldest pending job or any pending jobs ready for execution. For example, an index on `(status, next_attempt_at, created_at)` allows selecting the next jobs where status is `Pending` and `next_attempt_at` is due, ordered by enqueue time. Alternatively, maintaining a separate boolean or status filter is fine. We also index `tenant_id` if we need to query jobs per tenant (for troubleshooting or if we ever limit how many jobs a single tenant has enqueued). The primary key (job\_id) ensures uniqueness and can be used to directly address jobs for updates.

**Content Storage:** The queue table stores the **fully rendered content** of the message at enqueue time – e.g., the exact email body text, SMS text, or webhook payload as it should be sent. This design ensures that the sending process does not depend on live database data or templates at send time, avoiding inconsistencies. By rendering content up front, we snapshot the message so that any subsequent changes (to a template or record) won’t affect the outgoing message, and the worker can send it without additional lookups. The content is duplicated in the tenant’s history (see below) for record-keeping. In an alternative design, the queue could store just a reference (like a foreign key to a `messages` table per tenant), but that would require cross-database transactions or two-phase commits. To simplify and ensure the queue job is self-contained for processing, we include necessary content fields directly in the global queue table at the cost of minor duplication.

### Per-Tenant Message History Table

Each tenant has its own **message history** table (for example, `message_history`) in the tenant’s database/schema. This table records all messages sent or attempted for that tenant, serving as a log that tenant-specific features (e.g. an admin UI or audit trail) can query. By storing this per tenant, we maintain data isolation – one tenant cannot access another’s message records – and it scales with the number of tenants (each table contains only that tenant’s data). The **message\_history** table is updated as jobs progress and complete. Key fields likely include:

* **message\_id** (BIGINT or UUID): Primary key for the message record in the tenant’s context. This could be distinct from the global job\_id, but to simplify tracking, we can use the same ID in both places (the enqueue API can create both records and use one ID for both). Alternatively, message\_id can be tenant-local and the global queue stores it as a reference.
* **channel** (ENUM/VARCHAR): The channel of the message (e.g. Email, SMS, Push, Webhook), same as in the queue.
* **to\_address** (TEXT): Recipient or target, same as queue.
* **subject** (TEXT, nullable): Subject or title, if applicable.
* **body\_text** (TEXT): Text content of the message.
* **body\_html** (TEXT, nullable): HTML content (for email).
* **attachments** (JSON/TEXT, nullable): Attachment list or references, if any.
* **status** (ENUM or VARCHAR): The final status of the message delivery for this tenant record. Possible values: `Delivered` (sent successfully), `Failed`, or in some cases `Pending`/`Retrying` if we log intermediate state. This status is updated as the message is tried and eventually succeeds or fails.
* **attempt\_count** (INT): Total number of attempts made. This is updated along with the status; if a message ultimately succeeded on the 3rd try, attempt\_count becomes 3.
* **first\_attempt\_at** (TIMESTAMPTZ): Timestamp of when the message was first enqueued (could reuse `created_at`).
* **last\_attempt\_at** (TIMESTAMPTZ): Timestamp of the most recent attempt to send.
* **sent\_at** (TIMESTAMPTZ, nullable): Timestamp when the message was successfully sent (set if status = Delivered). This could reflect when our system successfully handed off to the provider.
* **failed\_at** (TIMESTAMPTZ, nullable): Timestamp when the message was marked as failed (after final attempt).
* **error\_message** (TEXT, nullable): If the final outcome was failure, this field can store a short description of the error or last error encountered (for troubleshooting). For example: “SMTP 550 recipient not found” or “Twilio API error: code 21608 (number blacklisted)”.
* **provider\_msg\_id** (VARCHAR, nullable): An identifier returned by the provider, if available. For example, SendGrid’s API might return a message ID, Twilio returns an SID for the SMS, FCM returns a message ID for push, etc. Storing it allows correlation with provider logs or webhook callbacks.
* **created\_at** (TIMESTAMP): When the history record was created (should match enqueue time).
* **updated\_at** (TIMESTAMP): When the record was last updated (on status change).

On enqueue, a new row is inserted into the tenant’s `message_history` with status `Pending` (or `Queued`). Each retry or status change triggers an update to this record (incrementing attempt\_count, changing status to `Failed` or `Delivered`, etc., and timestamps). Upon final success or failure, the record reflects the final state and retains the content for reference. Because the content and metadata are stored here, the tenant’s administrators can later query what messages were sent, when, to whom, and with what content – important for support and audit purposes.

**Relationship to Queue:** The global queue and the tenant history are kept in sync through the processing lifecycle:

* When a message is enqueued, both a queue job and a history record are created. They share a common identifier or have a clear mapping (e.g. the queue stores `tenant_id` + a reference to message\_id, or we use the same UUID for both).
* The global queue entry is transient – it will be removed or flagged done after processing – whereas the history record persists long-term.
* The worker, after sending, updates the history in the tenant DB as part of the completion transaction (or a separate transaction just after completion). If the tenant database update fails for some reason (e.g. the tenant DB is temporarily unreachable), the system should have a mechanism to retry updating the history or mark the job such that it can be reconciled later. In practice, if a future CockroachDB deployment uses shared schemas, we could design the schema such that the history is also in a globally accessible form. But given our architecture, it’s likely each tenant has a separate connection (which could be a different PostgreSQL schema or a different database). Ensuring atomic update of both global and tenant data is a challenge – distributed transactions could handle multi-table transactions across schemas if properly configured. If not, we accept eventual consistency: e.g. update the global queue first, then update history. Minor inconsistencies (like a message marked delivered in queue but history not updated) can be corrected by a reconciliation job or simply by the fact that the queue is ephemeral (if the history wasn’t updated, the message can be requeued or logged as error for manual fix).

**Opt-Out / Unsubscribe Table:** In addition to message\_history, each tenant will have an **opt\_out** table (e.g. `message_opt_out`) to track recipients who should not be contacted. This table typically contains fields like contact (`email_address` or `phone_number`), opt-out type (maybe channel or all), and timestamps/reason. Before sending a message, the system will check this table to ensure the recipient has not unsubscribed or been globally suppressed. For emails, this is crucial for CAN-SPAM compliance (users who clicked “unsubscribe” should not get further emails), and for SMS it’s required to honor STOP requests. The **Enqueue** API can perform this check upfront – if a target is in opt\_out, the message might be rejected from queuing (or queued with a special status that immediately marks it as canceled). At send time, the worker will also double-check (especially for recurring or delayed sends) that the recipient hasn’t been added to opt-out in the interim, to avoid sending against a new opt-out. The opt-out list is maintained per tenant (since each tenant manages its audience separately), but for some channels like SMS, there are also global carrier-level blocks; our system primarily handles the tenant’s own suppression lists (and can integrate provider-level suppressions via webhooks in the future).

## Worker Coordination and Job Processing Algorithm

**Job Claiming:** Each node’s messaging worker pool constantly looks for jobs that are ready to send. Workers issue a query against the global `message_queue` table for the next available job. A simplified logic flow:

1. **Fetch Pending Job:** The worker executes a query to select one pending job. This can be done with a locking read, for example:

   ```sql
   BEGIN;
   SELECT * 
   FROM message_queue 
   WHERE status = 'Pending' AND next_attempt_at <= now()
   ORDER BY priority ASC, next_attempt_at ASC, created_at ASC 
   LIMIT 1 
   FOR UPDATE SKIP LOCKED;
   -- (SKIP LOCKED is used if supported, to ignore rows already locked by other transactions)
   ```

   This will lock the selected job so no other worker can take it. If no job is found, the worker waits or polls again after a short delay.
2. **Mark as In-Progress:** If a job is fetched, the worker marks it as claimed by updating its status (and optionally recording which node/worker claimed it). For example:

   ```sql
   UPDATE message_queue 
   SET status = 'Processing', claimed_by = '<node-id>', claimed_at = now() 
   WHERE job_id = <selected_job_id>;
   COMMIT;
   ```

   At commit, this transaction finalizes the claim. The job is now marked as processing by this node. (The `claimed_by` could be the server’s instance ID or hostname for debugging.)
3. **Load Tenant Context:** The worker identifies the tenant via `tenant_id` in the job and ensures the tenant’s context is loaded (database connection, credentials, etc.). Adept may have a **Tenant LRU cache** that loads on demand. If the tenant is not already in memory, the worker will load the tenant (opening a DB pool to that tenant’s schema) so that it can update the tenant’s message\_history and access tenant-specific settings (like API credentials or opt-out lists).
4. **Prepare Message Data:** The worker constructs a message object from the queue record. Since content is already rendered, this usually means mapping the DB fields into an internal struct like `EmailMessage`, `SMSMessage`, etc. It may also retrieve any attachments (e.g., reading files from disk or object storage if the attachment field contains file paths or URLs).
5. **Opt-Out Check:** Just before sending, the worker performs a final opt-out check (for email/SMS). It queries the tenant’s `opt_out` table for a matching contact. If the recipient has opted out since the job was enqueued, the worker will **abort the send**:

   * It updates the job’s status to “Canceled” (or simply deletes it) and updates the history record with status “Canceled/Opted-Out” and no attempts. This prevents sending. Then it logs the event (so we know a send was skipped due to opt-out) and moves on.
     If no opt-out is found, proceed to send.
6. **Send via Provider Adapter:** The worker calls the appropriate **provider adapter** for the channel (details in next section). This is done outside of any database transaction (since it involves network I/O to an external service). The adapter will use the tenant’s credentials and the message data to perform the send:

   * **Email example:** call SendGrid API to send the email (with To, Subject, Body, Attachments, etc.).
   * **SMS example:** call Twilio API to send the SMS.
   * **Push example:** call FCM/APNS to send a push notification.
   * **Webhook example:** perform the HTTP POST to the given URL with the payload.
     During this step, the subsystem should handle any exceptions or errors the provider returns. The call may take some time (usually a fraction of a second, but could be seconds if network latency or if waiting on provider).
7. **Handle Send Result:** Once the adapter returns, the worker inspects the result:

   * If **successful:** (the provider accepted the message), the job is considered complete. The worker will record success.
   * If **failed:** (the provider or network returned an error), determine if it’s a transient failure or permanent:

     * Transient errors (e.g. network timeouts, 5xx server errors from provider) will be retried.
     * Permanent errors (e.g. "invalid email address", "user is blacklisted", or an HTTP 400 for bad webhook payload) should not be retried.
       The adapter can classify the error or provide an error code to help make this decision.
8. **Finalize Job Transaction:** The worker opens a new transaction (to the global DB and tenant DB, possibly two-phase if needed) to update the status accordingly:

   * For a **successful send**:
     a. **Global queue:** Delete the job row (or mark status = `Succeeded` and perhaps keep it for a short period if we want to retain it temporarily). Removing it frees the queue for other jobs and avoids re-processing.
     b. **Tenant history:** Update the message\_history record for that message: set status to “Delivered”, attempt\_count, sent\_at = now, and record the provider’s message ID or any delivery info. Also clear any error message that might have been there from previous attempts.
   * For a **failed attempt (not final)**:
     a. **Global queue:** Increment `attempt_count`, compute a new `next_attempt_at` based on the backoff schedule, and set `status = 'Pending'` again (or keep it as pending but effectively it remains in queue with a future send time). Optionally, store the last error message in a field for visibility.
     b. **Tenant history:** Update attempt\_count and perhaps status = “Retrying” (or keep as Pending but with updated attempt count). Log the error message in the history (so tenant admins can see the reason if it ultimately fails or is taking multiple tries).
     The job will remain in the queue for the next attempt. The worker does **not** delete it in this case. The transaction commits these changes. The message will be picked up again by a worker after `next_attempt_at` has passed.
   * For a **permanent failure (no more retries or non-retryable error)**:
     a. **Global queue:** Delete the job (or mark as `Failed` and delete shortly after).
     b. **Tenant history:** Update status to “Failed”, attempt\_count, failed\_at = now, and log the error reason.
     At this point, no further retries will occur. The message is considered closed out with failure.
9. **Post-processing:** After committing the final transaction, the worker may emit logs and metrics (covered in Observability below). It then returns to step 1 to fetch the next job.

This loop runs continuously on all active nodes. The use of `SELECT ... FOR UPDATE` (with SKIP LOCKED) or similar means workers naturally distribute jobs without conflict – each job will be locked by one worker and others will skip it. The result is a **scalable, contention-minimized queue**: if there are N pending jobs and M workers, at most M jobs are locked at a time, and others remain available.

**Throughput and Scaling:** If the volume of messages grows, we can increase the number of worker goroutines per node and/or add more nodes. CockroachDB will handle the increased transaction load on the queue table. We should monitor the queue length; if it consistently grows, that’s a sign to scale out or tune the send rate (or consider using an external queue). The roadmap notes that a future enhancement (MVP-1.0) is a dedicated “job runner with retries” – the current design essentially serves that purpose within the DB, but we keep an eye on whether using a more specialized queue backend (like NATS/JetStream or Kafka, which were considered) becomes necessary. At MVP, however, the CockroachDB global queue is preferred for simplicity and consistency within transactions.

## Provider Adapters and Channel Implementations

The messaging subsystem uses a **pluggable provider adapter** model for each channel. This means the core queue/worker logic is decoupled from the specific email or SMS service – we define an interface for sending a message of a given type, and different providers (SendGrid, AWS SES, Twilio, etc.) can implement that interface. This aligns with Adept’s general approach of provider-agnostic integrations (similar to the AI provider router for OpenAI, etc.). Each tenant can potentially use a different provider (if configured) or the system can switch out providers without changing the high-level messaging code. Initially, we will implement **SendGrid** as the default email provider and **Twilio** for SMS, with placeholders or plans for others. Push notifications will be added in Phase 2 (using FCM/APNS), and webhooks are implemented with a generic HTTP client.

### Adapter Interface Design

We define a set of interfaces or a unified interface for message sending. One approach is a generic interface, e.g.:

```go
type MessageAdapter interface {
    SendEmail(ctx context.Context, msg EmailMessage) error
    SendSMS(ctx context.Context, msg SMSMessage) error
    SendPush(ctx context.Context, msg PushMessage) error
    SendWebhook(ctx context.Context, msg WebhookMessage) error
}
```

However, since each channel might need different initialization (API keys, endpoints) and response handling, it can be clearer to have separate interfaces or structs for each channel’s provider, all implementing a common pattern. For instance, an `EmailProvider` interface for sending emails, an `SMSProvider` for texts, etc. Under the hood, though, all adapters will adhere to the same high-level contract: take a fully-formed message and attempt to deliver it, returning either success or a detailed error.

Adept’s internal `api` package is used to manage third-party service clients and credentials. We will integrate the provider adapters with this layer. For example, a tenant’s SendGrid API key will be loaded into `Tenant.API["sendgrid"]` (as a SendGrid client or token) by the credentials loader, so the messaging subsystem can retrieve it easily. This avoids hardcoding secrets and allows central management of API clients (with features like built-in retry, rate limiting, etc., from the API layer). If a tenant chooses a different provider (say AWS SES for email), their `Tenant.API["ses"]` would be set and we could dynamically choose the SES adapter instead of SendGrid for that tenant.

### Email Channel Implementation

For email, the subsystem provides an **EmailMessage** struct and uses an **EmailAdapter** (provider) to send it. The EmailMessage includes fields like: `To` (recipient email address), `From` (sender address or name, possibly defaulted per tenant or message), `Subject`, `BodyText`, `BodyHTML`, and `Attachments`. Attachments might be represented as file paths or byte data plus MIME types (the adapter will handle reading files and encoding them).

**SendGrid Adapter:** In Phase 1, we implement the adapter for SendGrid’s API. This adapter will use the SendGrid REST API (or official Go SDK) to send the email. It will take the EmailMessage fields and construct an API request (including handling multiple recipients, CC/BCC if needed, attachments as base64 content, etc.). The adapter will use the tenant’s SendGrid API key (fetched via Vault at startup and cached in the tenant context) to authenticate. On success, SendGrid typically returns a 202 Accepted status with an ID in the response headers; the adapter can treat any 200-level response as success (no need to parse ID unless needed for tracking – optionally we can capture the SendGrid Message-ID for the history). On failure, the adapter will parse the error (SendGrid might return 4xx for bad requests like invalid emails or 5xx for service issues). It will map these to an `error` with either a specific type or message. For example, if the response indicates an invalid recipient, the adapter might return an `ErrPermanent{Reason: "..."} ` that our worker can recognize as non-retryable. Temporary errors (like a 500 or timeout connecting to SendGrid) would return a generic error or a custom `ErrTransient` type.

The EmailAdapter interface could also support other providers in the future:

* **AWS SES Adapter:** using AWS SDK to send email (could be added later).
* **SMTP Adapter:** for tenants that prefer SMTP, an adapter that uses an SMTP library to send via given SMTP server credentials. (Not in initial scope, but architecture allows plugging it in.)
* **Additional**: Providers like Mailgun, SparkPost, etc., can be added by implementing the same interface.

The system might allow per-tenant configuration: e.g., Tenant A uses SendGrid (so Tenant.A API keys loaded), Tenant B uses SES (Tenant.B API keys loaded and a config flag to select SES adapter). The messaging subsystem can pick the provider based on tenant settings. In Phase 1, if no per-tenant choice is implemented, we use the global default provider for all emails (SendGrid), but code is structured to make this configurable.

### SMS Channel Implementation

For SMS messages, we define an **SMSMessage** struct containing: `To` (recipient phone number, E.164 format), `From` (sender number or shortcode, typically configured per tenant or globally), and `Body` (text content, usually up to 160 characters for a single SMS, beyond which Twilio will segment or require MMS if attachments in future). The SMS adapter will be responsible for sending this via an SMS gateway.

**Twilio Adapter:** Phase 1 will implement SMS sending via Twilio’s Programmable SMS API. The Twilio adapter will use the Twilio REST API (via a Go SDK or direct HTTP calls) requiring the tenant’s Twilio Account SID, Auth Token (for API authentication), and a From number (Twilio phone number) to send from. These credentials are stored in Vault and loaded into `Tenant.API["twilio"]`. The adapter forms a request to Twilio’s `/Messages` endpoint with `To`, `From`, and `Body`. On success, Twilio returns a Message SID and a status (initially queued or sent). The adapter will treat a successful API call (HTTP 201 Created) as a success and return the Message SID for logging. On failure, Twilio returns an error code and message; e.g. code 21614 for “Invalid mobile number” (permanent) or 21611 for “Blacklisted number” (permanent), 30005 for carrier failure (which might be temporary). The adapter will interpret these codes: it can maintain a list of non-retryable error codes (like invalid number) and return an error signaling no retry. Others (like timeouts connecting to Twilio, or a 500 from Twilio) would be treated as transient.

Just like email, this adapter is swappable. In the future, if we integrate another SMS provider (say **Nexmo/Vonage** or **AWS SNS**), we’d implement an adapter for it. The system could even send via different providers based on region or cost, if extended, but initially one global choice is fine.

### Push Notification Channel Implementation

(*Planned for Phase 2*) Push notifications include mobile push (iOS, Android) and possibly web/desktop push. In the interim, the design will consider push but the actual implementation will come later. We plan for a **PushMessage** struct with fields: `DeviceToken` (or a list of tokens for broadcast), `Platform` (e.g. iOS, Android, Web), `Title`, `Body`, and possibly a `Data` payload (key-value pairs for additional info). There could also be a `Topic` or `Group` field if sending to a topic subscription (FCM supports topics).

We anticipate two main providers:

* **FCM (Firebase Cloud Messaging)** for Android (and potentially iOS and web via Firebase). FCM can send to both Android and iOS apps if the app is configured with Firebase.
* **APNS (Apple Push Notification Service)** for iOS directly. If not using FCM for iOS, we’d need to send via APNS using Apple Push credentials (p8 key or certificate).

We will likely abstract push sending behind a single adapter that chooses the route:
For example, a **PushAdapter** could detect platform: if platform is iOS and we have APNS credentials for tenant, use APNS; if Android or iOS-with-FCM, use FCM credentials. Alternatively, maintain separate adapters and have the worker logic call the appropriate one.

**FCM Adapter:** Would use the Firebase messaging API with the tenant’s FCM server key (or service account). We’d send a notification with title/body and optional data. FCM returns a message ID or an error. Failures like invalid registration token (permanent for that token) or server unavailable (transient) would be handled accordingly.

**APNS Adapter:** Would use Apple’s HTTP/2 API for push. It requires the device token and an APNS topic (app bundle ID). Apple responses indicate success or give an error reason (like "Unregistered" if the device token is invalid, meaning the app is uninstalled – permanent error, no retry).

**Desktop/Web Push:** If we implement web push notifications (for PWA or web browsers), we’d use the Web Push protocol (which involves sending to an endpoint provided by the browser, with an encrypted payload). That might require the tenant to have VAPID keys. This is more specialized and can be treated as a separate channel or integrated into Push with platform "Web". For now, we note the system is extensible to that, but not in Phase 1 or 2 unless there’s specific demand.

All push notifications share the idea that they might not be guaranteed delivered (device offline, etc.), and the providers typically do not support retries from our side beyond sending the message (the push services handle their own retries to the device). However, if a send attempt to the push service fails due to network issues, we will retry similarly to other channels.

### Webhook Channel Implementation

Webhooks allow the system to perform an HTTP request to an external service as a form of notification or integration (for example, posting form submission data to a partner’s API). In the messaging subsystem, a **WebhookMessage** could include: `URL` (the endpoint to call), `Method` (likely just POST in most cases, but could support GET, PUT if needed), `Body` (the payload to send, often JSON or form data), and perhaps `Headers` (any custom headers, e.g. for authentication at the target).

The **Webhook Adapter** will be relatively straightforward: it will use an HTTP client to make the request. We will enforce some security measures:

* **HTTPS only:** By default, the adapter should require the URL to be HTTPS (to ensure data is sent securely). Non-SSL endpoints could be allowed via config if necessary for internal use, but generally not for external integration partners.
* **Timeouts:** The request will have a timeout (e.g. a few seconds) to avoid hanging a worker for too long. If the timeout is reached or connection fails, that’s a transient error for retry.
* **Response handling:** If the remote endpoint returns a 2xx status, we consider it success. Any 4xx or 5xx status is considered a failure. We may capture the status code and a snippet of the response in the error message for logging. We **do not** retry 400-series errors except maybe 429 Too Many Requests (which implies a need to retry after some delay given by e.g. a RateLimit header). A 401 Unauthorized or 403 Forbidden is likely a configuration issue (bad auth) – treat as permanent failure unless credentials change.
* **Auth**: The adapter can support custom headers for authentication tokens or basic auth. In practice, the system invoking the webhook might include an auth header in the payload (the EnqueueWebhook API can allow specifying headers).
* **Verification**: If integration partners require verifying the source, we could include a signature header. For example, generate an HMAC of the payload with a tenant-specific secret and send it as `X-Adept-Signature`. The receiving endpoint can verify that to ensure the request indeed came from Adept and not someone else. This is an optional feature for security – the spec should note it as a recommended practice for secure integrations.

Because webhooks are essentially arbitrary external calls, we must be careful to prevent abuse. Tenants could potentially enqueue webhooks to internal or sensitive URLs (SSRFi attacks). To mitigate this, the system could validate the URL against a safe-list or block private IP ranges. For now, we document that **the webhook adapter should not allow requests to private network destinations** (e.g. `10.x.x.x`, `192.168.x.x`, etc.) unless explicitly configured for a trusted integration. This ensures one tenant cannot use the system to probe Adept’s internal network or AWS metadata, for instance.

The webhook adapter is “universal” in the sense that unlike email or SMS where multiple providers exist, webhooks are just direct HTTP – so there isn’t a need for pluggable providers. However, if in future we integrate with specific workflow engines or services, they could be treated as special cases of webhooks or separate channels.

### Unified Extensibility

All channel adapters implement sending in a consistent manner, returning either success or an error that the worker can interpret. Adding a new channel (say **Voice calls** or an **OTT messaging** service like WhatsApp) would follow the same pattern:

* Define a new message type struct (e.g. `CallMessage` with fields like `To` number, optional `VoiceTemplate` or text-to-speech content).
* Add a new channel enum value (`Voice`).
* Implement an adapter (e.g. Twilio Voice adapter) to initiate calls. This might involve making a call via Twilio’s Voice API or another provider, possibly playing a certain message or connecting to a conference. The job queue and history can accommodate it by storing the necessary fields (for a voice call, the “body” might be a script or template ID).
* Integrate it into the worker logic (dispatch to the new adapter when channel == Voice).
* Include any new provider credentials in the `Tenant.API` loading (e.g. Twilio is already there for SMS; same credentials could be used for voice).

Similarly, adding a channel for **WhatsApp** (which Twilio also supports via its API) might be done by extending the SMS adapter to handle WhatsApp messages (they are sent via Twilio by using `whatsapp:` prefix on numbers). Alternatively, treat it as separate channel with its own adapter if using a different provider. The architecture is flexible – the queue is generic over channels, and the adapter model means minimal changes to support new outbound methods.

## Observability and Monitoring

The messaging subsystem includes robust observability features to ensure we can monitor delivery outcomes and performance. This includes **logging**, **metrics**, and **status tracking** dashboards.

### Logging

Every significant event in the message processing flow is logged via Adept’s structured logging (Zap logger). Logs are emitted at appropriate levels:

* **Info level** for successful sends (e.g. “Email sent successfully” with context).
* **Warn or Error level** for failures or retries (e.g. “SMS send failed – will retry”, including error details).
* Each log entry includes key context fields: tenant identifier (so we can filter logs per tenant), message ID or job ID, channel, and provider. It will not include full message content or PII in plaintext in order to protect sensitive data (for example, log email addresses in masked form or not at all; instead log user IDs or just domain).
* When a job is retried or finally fails, the log will note how many attempts were made and the error cause.

We also integrate logs with tracing if enabled: since the subsystem might be triggered from an incoming request (e.g. a form submission that calls EnqueueEmail), we can propagate trace/span context so that sending the message can be part of an overall trace. However, much of the work is async – we might start a new trace for the background processing. The `observability` package’s tracing integration (OpenTelemetry) could be used to mark spans for “message send attempt” including tags like message ID, channel, provider, outcome.

### Metrics

We publish Prometheus metrics to give visibility into the messaging system behavior. Proposed metrics:

* **Total Sent Messages:** `adept_msg_sent_total` – counter of messages successfully sent, labeled by channel (e.g. `channel="email|sms|push|webhook"`). Every time a message is delivered on final success, this increments.
* **Total Failed Messages:** `adept_msg_failed_total` – counter of messages that ultimately failed (gave up after retries), labeled by channel. This helps identify if certain channels are experiencing problems.
* **Retries Count:** `adept_msg_retry_total` – counter for each retry attempt (or total retries that occurred). Possibly labeled by channel and maybe by reason (transient vs permanent), if we want to see how many retries happen.
* **In-Progress Gauge:** `adept_msg_inflight` (gauge) – number of messages currently being processed. Possibly one could derive this indirectly, but a gauge updated by workers when they start/finish could track concurrency.
* **Queue Length:** We can periodically measure the number of pending jobs. Because this is just a DB count, we might not have it as a live metric unless we poll. We could create a gauge `adept_msg_queue_length` updated every X seconds by one node (or as part of a health check) to reflect how many jobs are in `Pending`. This is useful to detect backlog buildup.
* **Latency Histogram:** `adept_msg_send_duration_seconds` – a histogram of the end-to-end send duration per message (from the time it was dequeued to the time the provider accepted it). This can be labeled by channel and provider. It measures the performance of external sends plus any processing overhead. We might also measure total time in queue (enqueue to final send) to monitor if messages are being delayed, but that can be derived by comparing timestamps in history if needed.
* **Provider-specific metrics:** Optionally, track success/fail per provider (if multiple providers per channel). For now, channel covers it since each channel currently has one provider, but if we add alternatives (e.g. SES vs SendGrid), we might label by `provider="sendgrid|ses|..."` as well.

All metrics use Adept’s prefix and conventions (as seen with security and AI metrics). The metrics will be visible on the `/metrics` endpoint and can be integrated into dashboards. For example, we can set alerts if `adept_msg_failed_total` increases rapidly (indicating a send outage) or if `adept_msg_queue_length` stays high for too long (messages backing up).

### Status Tracking and Dashboards

The **message\_history** tables serve as a source for tracking delivery status at the application level. Integration partners or internal admin UIs can query these tables (via an API or direct DB read) to get the status of messages. For example, a tenant’s admin UI could show a “Message Log” listing emails and SMS sent, their status (Delivered/Failed/Queued), and timestamps. This is useful for support (e.g., “Did the user get the welcome email? Yes, it was sent on X date to Y address, provider confirmed delivery.”).

In future Phase 2, **provider webhook processing** will enhance status tracking. That is, for channels like email and SMS, the initial “send” might not guarantee delivery (e.g., an email can bounce, or an SMS might fail later). Providers often send webhook callbacks for events:

* SendGrid can send events for delivered, bounced, opened, etc.
* Twilio can send status callbacks when an SMS is delivered or if it fails after retries.
* APNS/FCM don’t have delivery confirmation at the device, but APNS can send a feedback about invalid tokens, etc.
  We plan to have endpoints or background processes that receive these callbacks and update the message\_history records with final dispositions (for instance, marking an email as Bounced if a bounce event comes in, or logging that an SMS was delivered to the handset). These would be additional fields (e.g. `delivered_at` might be set on provider callback if it happens after initial send) or separate tables for detailed delivery events. For this spec, the main focus is on initial send status; however, it’s architected such that **observability hooks can update message\_history asynchronously** without interfering with the core queue (since the history record can be updated any time with new info).

We will implement **Prometheus alerts** for critical conditions: e.g., if `adept_msg_failed_total` for email increases by a large amount in a short time, that could indicate our email provider key is invalid or their service is down – an alert can be fired. Likewise, a consistently large queue length or high age of oldest message could trigger an alert that the workers are not keeping up or stuck.

## Retry and Backoff Strategy

Reliable delivery is achieved via an **exponential backoff** retry strategy with a limit on attempts. The goal is to automatically retry transient failures while not overwhelming providers or spamming an address that is consistently failing. The strategy is as follows:

* **Initial Attempt:** When a job is first enqueued, attempt 1 occurs almost immediately (as soon as a worker picks it up). If it succeeds, we’re done.

* **Exponential Delay:** If attempt 1 fails with a potentially transient error, we schedule a retry. We use exponential backoff with jitter. For example: attempt 2 after 1 minute, attempt 3 after 2 minutes, attempt 4 after 5 minutes, attempt 5 after 15 minutes (these are illustrative). We can use a formula like `next_delay = base * 2^(attempt-2)` (so if base=1min, attempts: 2->1min, 3->2min, 4->4min, etc.) with some random jitter (e.g. +/- 20%) to avoid synchronization if many jobs failed at once. The actual schedule can be tuned; for instance, a common pattern is 1min, 5min, 15min, 1 hour, etc. up to a max.

* **Max Attempts:** By default, we might cap at 5 attempts (the 5th attempt is the last). This can be configurable per channel or per job. For example, email might try 5 times over a few hours, whereas for webhooks to critical endpoints we might try more times over a longer period (since those might be very important to deliver). In Phase 1, we’ll use a reasonable default across channels for simplicity (likely 3-5).

* **Permanent Failures:** Certain errors will not be retried at all – these set `max_attempts` effectively to the current attempt. For instance, if SendGrid returns "Invalid recipient domain" or Twilio returns "Unreachable carrier", these are permanent. The adapter will flag the error as non-recoverable, and the worker will mark the message as failed immediately (attempt\_count still increments, but we won’t schedule another attempt). The message\_history will reflect the failure reason so the tenant can take action (e.g., remove or correct the address).

* **Backoff Storage:** The `next_attempt_at` field in the queue table is crucial. After a failure, the worker will set `next_attempt_at = now() + delay` (with the appropriate delay for the attempt count) and leave status as `Pending`. This ensures the job won’t be picked up until that time. Workers will always skip jobs whose next\_attempt\_at is in the future. This design is more efficient than continuously polling a job that’s not ready, and it naturally staggers retries.

* **Jitter and Coordination:** We add randomness (jitter) to backoff to prevent all failing jobs from retrying in lockstep (which could cause a thundering herd effect on a provider or our DB). This is especially relevant if, say, SendGrid had a 5-minute outage and 1000 messages failed at 12:00 – without jitter, they’d all retry at 12:05. With jitter, they’d spread out roughly between 12:05 and 12:06 (for example), smoothing the load.

* **Manual Retries:** In some cases, an operator or tenant admin might want to manually trigger a retry sooner (for instance, after fixing a configuration issue). Although not implemented in Phase 1, the design permits it: an admin tool could update a message\_history entry and corresponding queue entry to set attempt\_count down or status back to pending and next\_attempt\_at = now, effectively requeueing it. Alternatively, if the queue entry was removed on failure, a manual “resend” action could create a fresh job (possibly referencing the original message content).

* **Giving Up:** When the attempt\_count reaches max\_attempts and the last attempt fails, the worker will mark the job as **failed** without scheduling another retry. At this point, it’s final. The job is removed from the queue and the history marked failed. No further automated action occurs. It’s then up to the tenant to handle the failure (e.g., perhaps notify an admin, or just log it).

* **Different Backoff per Channel:** We might configure different base delays or max attempts per channel:

  * Email: could have more attempts (since email can be retried later, maybe over hours).
  * SMS: if it fails due to carrier issues, Twilio often retries internally as well; we might not need too many app-level retries. Perhaps fewer attempts.
  * Webhook: if a webhook endpoint is down, we might try for an extended period (maybe over 24 hours with increasing intervals) if the data is critical to deliver, or conversely fewer attempts if not critical. This might be configurable by the component that enqueues (e.g., a form could specify how persistent to be).
  * Push: often not retried at app level because the push service queue handles it, but if our initial send to FCM/APNS failed, retry a couple times.
  * For simplicity in Phase 1, we implement a common mechanism but leave room for per-channel tuning later.

* **Interaction with Provider Limits:** If a provider has its own rate limits, the retry strategy should account for that. For example, if SendGrid API starts returning 429 (rate limit exceeded), our system should treat that as a transient error but also perhaps slow down globally for that provider. In Phase 1 we may rely on providers’ built-in handling and our general backoff. If needed, we can add a rate limiter in the adapter (the `internal/api` layer might already support a rate limit for the SendGrid client). Similarly, if Twilio has SMS send rate limits, the adapter can queue internally or return a specific error that we interpret to back off more aggressively. Observing metrics and provider documentation will guide refinements here.

All these strategies ensure that the system is **robust**: temporary failures (network blips, momentary provider issues) will not result in lost messages – they’ll be retried automatically, and permanent failures will be recorded clearly. The design favors delivering messages eventually rather than dropping them silently.

## Usage Examples of the Messaging API

Other components of Adept will use the messaging subsystem via a Go API, which provides convenience functions to enqueue messages. The API functions abstract away the queue details; a component simply creates a message object and calls enqueue. Below are examples for each channel:

* **Sending an Email:** Suppose the Auth component wants to send a welcome email to a new user. It would do something like:

  ```go
  import "github.com/yanizio/adept/internal/message"

  // ... inside some business logic
  email := message.EmailMessage{
      To:      "new.user@example.com",
      From:    "Acme Inc <no-reply@acme.com>",  // could be defaulted by system or tenant config
      Subject: "Welcome to Acme!",
      BodyText: plainTextBody,
      BodyHTML: htmlBody,
      Attachments: []message.Attachment{
          {Name: "welcome.pdf", Path: "/inet/sites/acme.com/assets/docs/welcome.pdf"},
      },
  }
  err := message.EnqueueEmail(ctx, tenantID, email)
  if err != nil {
      // handle error (e.g., log it; in most cases err is nil if queued successfully)
  }
  ```

  The `message.EnqueueEmail` function will internally do something like: load the tenant (tenantID), check basic validity (e.g., ensure `To` is not empty), render the body (in this example, plainTextBody and htmlBody are already provided – perhaps formatted using a template earlier), and insert a new record into the global `message_queue` and the tenant’s `message_history`. It likely uses a transaction to insert both, or inserts history first then queue. On success, the email is now queued for sending in the background, and the function returns. This call is asynchronous – the user’s original action (maybe an HTTP request to sign up) is not blocked waiting for the email to send; they can continue while the email is delivered in the background.

  The attachments in this example illustrate attaching a tenant-hosted file. By specifying the path to a file in the tenant’s assets (or a URL), the system knows where to retrieve the file when sending. The worker will load the file content from disk at send time and include it in the email (the Attachment struct could hold either a file path or already-loaded byte data and MIME type).

* **Sending an SMS:** For example, the Security component might send an SMS for two-factor authentication. Usage:

  ```go
  sms := message.SMSMessage{
      To:   "+14155551234",
      Body: "Your verification code is 123456",
  }
  err := message.EnqueueSMS(ctx, tenantID, sms)
  if err != nil {
      // if queue insert failed
  }
  ```

  The SMSMessage may not need a From here because the system could default to the tenant’s configured Twilio number or sender ID. EnqueueSMS performs similar steps: insert into queue (with channel = SMS, content as given) and history. This SMS will be picked up and sent via Twilio. Because SMS are short and without attachments, the process is simpler. The history will show the text that was sent and whether it succeeded or not. (For 2FA codes, even if it fails, the user may retry, but logging is still useful.)

* **Sending a Push Notification:** (In Phase 2, but demonstrating usage) Suppose an admin wants to send a broadcast push to all users of their mobile app:

  ```go
  push := message.PushMessage{
      DeviceToken: "<device-token>",
      Platform:    "ios",
      Title:       "New Feature",
      Body:        "Check out the new dashboard feature we added!",
      Data:        map[string]string{"screen": "dashboard"},  // custom payload data
  }
  err := message.EnqueuePush(ctx, tenantID, push)
  ```

  This would queue a push notification. In reality, if broadcasting to many devices, the component might loop or call a specialized multicast function, but each push might be enqueued as a separate job (unless using a provider’s multicast topic feature). EnqueuePush adds a job with channel=Push. The worker will later send it via FCM or APNS as appropriate.

* **Enqueuing a Webhook call:** For a form submission scenario, the Forms component could have a webhook action configured. When the form is submitted and validated, it would do:

  ```go
  webhook := message.WebhookMessage{
      URL:    "https://partner.example.com/api/lead",
      Method: "POST",
      Body:   formPayloadJSON,  // e.g., `{"name": "Alice", "email": "alice@example.com", ...}`
      Headers: map[string]string{
          "Content-Type": "application/json",
          "X-Api-Key": "<tenant's partner API key>",
      },
  }
  err := message.EnqueueWebhook(ctx, tenantID, webhook)
  ```

  This will queue an outbound HTTP POST to the given URL with the specified JSON body and headers. The form submission request itself can return immediately (perhaps showing the user a "Thank you" page), while the actual integration call happens asynchronously. The message\_history for this tenant will log the webhook attempt and whether it succeeded (e.g., HTTP 200) or failed (non-200 or error). If it fails, the retry mechanism will try again according to configured backoff.

In all these examples, the enqueue functions abstract the details of constructing the job. They likely validate inputs (e.g., ensure the email address format is valid, phone number is in proper format, URL is parseable and allowed, etc.) to catch errors early. They might also apply some normalization (like lowercasing emails, stripping risky headers in webhooks). Any errors at this stage (like “invalid phone number” or “payload too large”) would be returned immediately to the caller so the calling component can handle it (maybe returning an error message to the user or logging a misconfiguration).

The API is designed to be **easy to use (one call to send a message)** and to encourage asynchronous patterns. By using context (the `ctx` parameter), we also propagate cancellation or deadlines if needed (though typically these go routines will ignore context cancellation once queued). The context is more relevant for the DB operations on enqueue.

Integration partners using Adept (for example, a tenant who develops a custom component) will also use these same APIs to send notifications, ensuring consistency and proper logging. The internal nature of the API means only server-side code can enqueue messages – which is good for security (no external entity can directly spam the queue without going through validation/business logic).

Finally, note that **idempotency**: if a component calls EnqueueEmail twice by mistake (perhaps due to a retry of a user request), it will result in duplicate emails unless the component itself guards against it. The messaging subsystem will treat them as separate jobs. In future, we might add an optional deduplication key to the API (for instance, a message ID provided by the caller) to avoid accidental dupes, but that’s not in scope initially.

## Extensibility and Future Enhancements

The design of the messaging subsystem is forward-looking, allowing new channels and features to be added with minimal disruption. Some extensibility notes:

* **Voice Calls:** As mentioned, we can extend to voice calling. This could be used for phone call alerts or phone-based OTPs. Implementing this would involve integrating with a provider like Twilio (Voice API) or Nexmo Voice. The queue and history can accommodate a “Voice” channel. We would need to store the target phone number and either a pre-recorded message or a text-to-speech script. A Twilio Voice adapter might create a call and either use Twilio’s XML callbacks (TwiML) to say a message or connect the call to a pre-set message flow. This requires a bit more logic (like what happens when the person answers – likely for notifications we’d just play a message). From a subsystem perspective, however, it’s just another job that gets triggered and a result (call initiated or failed). We would mark it delivered once the call is successfully initiated (or perhaps when completed, if we track call status via webhook events).
* **OTT Messaging (WhatsApp, Telegram, etc.):** These can be added similarly. WhatsApp through Twilio is essentially sending an SMS with a specific channel identifier. We could either treat WhatsApp as a separate channel or as part of SMS (with a flag). The safer approach is a separate channel like “WhatsApp” with its own adapter, because content formatting might differ (WhatsApp allows templates, etc.). Telegram or others might require using their specific APIs/bots, again doable via an adapter. The queue could hold e.g. a chat ID instead of a phone number for such channels.
* **Email Template Management:** In Phase 2, Adept plans to introduce a **Template editor and i18n**. This will allow users to design email/SMS templates within the system and possibly send templated messages without coding each message’s content. The messaging subsystem will then integrate with a **template engine**. For example, instead of the caller passing a fully rendered body, they might pass a template ID and data model. The subsystem (or a pre-processing step in enqueue) would fetch the template and render it (possibly using the same templating system as the rest of Adept, or a separate template language for emails). We have to ensure the template is rendered at enqueue time (to follow the design principle of stored content). This means the `message.EnqueueEmailTemplate(ctx, tenantID, templateName, data, ...)` would internally do the rendering and then call EnqueueEmail with the result. Template storage might be in the database or filesystem, but likely in the tenant’s template store. Internationalization: could allow template variants per locale; the enqueue API might select the locale based on user preferences.
* **In-App Messaging:** Another future feature is “in-app notifications.” This blurs the line with push, but essentially means if the tenant’s application has a notion of notifications (like a bell icon that shows messages), Adept might store a notification in the database for the user instead of or in addition to sending a push. While not exactly an outbound channel, we could leverage the same subsystem: e.g., a channel type “InApp” could simply mean writing an entry to a `user_notifications` table (which a client app or web app checks). This could be handled by a lightweight adapter that writes to the DB (which might be overkill to go through the queue, but if we want to ensure uniform interface and possibly handle a large volume of notifications asynchronously, it could make sense).
* **Scheduled and Recurring Jobs:** The messaging subsystem can be generalized to handle scheduled sends or cron jobs. The roadmap hints at a “background job runner (CRON expressions) leveraging Messaging for digest emails”. We can leverage the global queue to schedule jobs in the future:

  * The `next_attempt_at` can be set far in the future for a scheduled message (and no retry unless missed).
  * For recurring jobs, a separate scheduler component could enqueue jobs at the specified times.
  * Digest emails: e.g., daily summary emails can be implemented by scheduling a job for each day. The messaging queue’s reliability and multi-node coordination ensure it gets sent once.
  * This doesn’t require changes to the core design, just using the fields appropriately.
* **Admin Tools and Integration:** For integration partners or internal ops, we might build API endpoints or CLI tools to manage the message queue:

  * Querying queue status (e.g., “how many jobs are pending for tenant X?” or “show me details of job Y”).
  * Forcing a retry or canceling a job.
  * These can be done by simple SQL or via an exposed admin API. We likely will not expose this publicly to tenants (other than via their message\_history view), but internally for support.
  * Also, as mentioned, receiving provider webhooks to update delivery status is an integration that will be implemented. This means opening endpoints (like `/webhook/sendgrid` to receive events, or Twilio status callbacks) and mapping those to message\_history updates and possibly emitting events to tenant apps if needed.
* **Switching Queue Backend:** Although CockroachDB works for now, if in the future throughput demands or architectural shifts favor a dedicated queue (like NATS JetStream or Kafka), the subsystem is modular enough to accommodate that. We would abstract the queue operations so that instead of writing to a DB table, Enqueue would publish a message to a queue, and a separate set of worker processes (or the same monolith nodes subscribing to topics) would consume and process. The rest of the logic – provider adapters, retries (which could be done via re-queuing with delay, e.g., using a delay queue feature or a scheduled message in a queue) – remains similar. For now, this is a note that the design is not irreversibly tied to Cockroach; it’s chosen for simplicity and consistency.

In summary, the messaging subsystem is built to accommodate new features without a rewrite: adding channels or providers is a matter of implementing new adapters and perhaps a few conditions in the worker, and many future capabilities (templates, scheduling, different storage backends) can be layered on while preserving the core queue+worker model.

## Security and Privacy Considerations

The messaging subsystem deals with potentially sensitive user data (email addresses, phone numbers, and message content) and external communications, so careful attention is given to security and privacy:

* **Tenant Data Isolation:** Each message is associated with a tenant, and data is stored in that tenant’s own schema for history. This ensures that if a SQL injection or bug were to expose message history, it would only ever expose the current tenant’s records, not all tenants’. The global queue table does contain entries from all tenants, but it is only accessible by the server (not exposed via any direct API to end-users). Access to that table is governed by application logic only. Internally, when updating history or loading tenant info, the system uses the tenant\_id to route to the correct database/schema.

* **Least Privilege in Credentials:** Provider credentials (API keys, tokens) are stored securely in Vault and loaded at runtime. The messaging subsystem never hard-codes secrets. For example, the SendGrid API key for tenant X is fetched from Vault (or the tenant’s encrypted config) and stored in memory only, in the `Tenant.API` struct. This means even if logs or errors are exposed, they won’t contain raw credentials. Additionally, the code should avoid including secrets in any error messages (e.g., if a Twilio request fails, do not log the URL with the auth token visible).

* **PII Handling:** Email addresses and phone numbers are Personally Identifiable Information. We must handle them with care:

  * In logs and metrics, avoid full exposure. For instance, we might hash or partially mask email addresses in logs. Or log just the domain part of an email, or an internal user ID if available. Phone numbers could be masked except last 4 digits.
  * The message content might also contain personal data (e.g., an email might have the user’s name, or a webhook payload might have form data). We do not log full content. The content is stored in the database (which is only accessible to authorized personnel and the application).
  * If required by compliance, we could encrypt certain content at rest. For now, we rely on database access controls and (if using CockroachDB) its encryption features. If a tenant requires their data at rest encryption beyond DB-level, we could encrypt message bodies with a tenant-specific key. That would complicate searching or debugging, so likely not implemented by default, but it’s a possible future enhancement for highly sensitive environments.
  * We will implement data retention policies if needed: e.g., maybe delete or truncate message bodies after X days if they contain sensitive info, while keeping metadata. (This is not planned for MVP, but worth noting for privacy regulations.)

* **Opt-Out Compliance:** We have built-in support for honoring unsubscribe requests. By maintaining per-tenant opt-out lists and checking them, we ensure compliance with laws and user preferences:

  * **Email:** Every bulk or marketing email should include an unsubscribe link that points to a handler which adds that email to the opt\_out table. The messaging subsystem will not send to addresses on that list. This helps comply with CAN-SPAM (USA) and similar laws globally. Additionally, if a tenant accidentally tries to send to someone on the list, the system prevents it, protecting both the user’s preference and the tenant’s sender reputation.
  * **SMS:** By US regulations, if a user replies "STOP" to an SMS, we must cease messages. Twilio can be configured to handle STOP automatically (it can reply with a STOP confirmation and not send further messages unless the user replies "START"). We should still add that number to our opt\_out if we receive such a signal (Twilio can notify via webhook when a STOP happens). Until Phase 2 webhook handling is done, we might rely on Twilio’s internal block and manually import those to opt\_out if needed. We instruct tenants (in documentation) that if they use our system to send SMS, they should enable Twilio’s compliance features or we implement them ourselves.
  * **Granularity:** The opt\_out could have a type (global opt-out vs channel-specific). For example, a user might opt out of marketing emails but still want transactional emails (password resets). In our implementation, we might classify messages and check opt-out accordingly. Initially, we treat opt\_out as absolute per channel (if email on list, skip all emails to that address from that tenant).

* **Secure Transport:** All outbound communications use secure channels:

  * Emails are sent via APIs (SendGrid) over HTTPS, or via SMTP with TLS if we ever go that route. We will enforce TLS for SMTP (and modern cipher requirements).
  * SMS are sent via HTTPS API to Twilio.
  * Webhooks must be HTTPS (with the rare exception allowed via config for specific internal endpoints). This prevents sending sensitive payloads in plaintext. We also encourage integration partners to use verification tokens as mentioned, to ensure authenticity of requests.
  * Push notifications: APNS and FCM communications are encrypted (APNS uses TLS/2, FCM uses HTTPS).
  * Thus, data in transit is protected.

* **Authorization & Abuse Prevention:** Because the messaging API can send to arbitrary addresses and URLs, we restrict who/what can call it:

  * Only server-side components with the proper context (and likely only Adept’s internal code, not user templates or unprivileged code) can invoke `message.Enqueue...`. For instance, if there’s a scripting or CMS feature, we would carefully expose messaging functions. We might not allow tenants to send arbitrarily to any address via a form without validation.
  * Rate limiting: We should ensure one tenant cannot overload the system or use it as a spam cannon. Adept’s global rate-limit middleware might throttle user actions that trigger messages, but we could also implement a cap like “no more than N messages per tenant per minute” in the enqueue function. This prevents abuse and accidents (like a bug spamming thousands of emails). Initially, this might not be in place, but we have it on the radar.
  * Size limits: Enforce reasonable limits on message size (email body, number of attachments, payload size for webhooks) to prevent excessive resource use. For instance, maybe limit attachments to, say, 10MB total per email, or webhook payloads to 1MB.

* **Privacy of Attachments/URLs:** When attaching tenant-hosted files, we ensure that these files are served or fetched securely:

  * If the file is on local disk (within the `/inet` deployment), workers have direct access. Only authorized system processes (the workers) read them, and they are sent out as needed. We should ensure the file reading is done with care (avoid following symlinks that could escape allowed directories, etc.).
  * If an attachment is a URL (like an S3 link or external URL), the system fetching it should do so over HTTPS and perhaps verify the domain if we restrict sources. Alternatively, we could require that tenant-hosted attachments be in known directories.
  * We should scan attachments or at least ensure we’re not sending malware unknowingly. Some systems integrate virus scanning for attachments. This might be beyond MVP scope, but a note: if tenants upload files that get emailed out, a virus scan (ClamAV etc.) might be advisable to protect recipients.
  * For links included in messages (like a link in an email), some tenants might want click tracking. That’s a feature we can add later (wrapping links to redirect through a tracking URL). For now, if any tracking needed, SendGrid can do open/click tracking if enabled in their account. We are not handling that directly yet.

* **Audit and Security Logging:** All send operations can be considered security-relevant in that abuse or mistakes can have legal impact. We maintain an audit trail via message\_history of what was sent. Admins (with proper authentication) should be able to see logs of outbound messages. Also, if a security event occurs (like an API key compromise leading to spam being sent), the audit trail helps investigate. We may integrate with Adept’s security engine to record if an outbound message was blocked due to being potentially malicious (though that’s an edge case; more relevant is preventing injection in email content).

  * One specific security measure: guard against **header injection** or **malicious content** in messages. For example, if we take user input and include it in an email header (like subject or To), ensure it’s sanitized so as not to break the format. Similarly for webhooks, ensure a malicious tenant can’t craft a request that does something unintended on our side (the risk is low since we’re the client, but for example, ensure we don’t follow redirects to file:// URLs or similar).

* **GDPR/Data Protection:** If users exercise “right to be forgotten,” we may need to delete their contact info from our logs. Because message\_history contains emails/phone numbers, we should design a way to anonymize or remove those on request. This could be a simple script to null out personal fields in history older than some time, or at least after a user deletion, ensure we update records (maybe replace email with a hash or placeholder). This is something to consider for compliance – for now, assume message history is part of business records and is retained as allowed, but be ready to scrub PII if needed.

In conclusion, the messaging subsystem is built with a strong security posture: using secure channels, respecting user privacy choices, protecting data at rest and in transit, and isolating tenant data. By following Adept’s conventions (e.g., Oxford comma in documentation, two spaces after periods as we maintain here, consistent component structuring) and integrating with existing systems (Vault for secrets, Observability for monitoring), this subsystem will be a reliable and secure foundation for all outbound communications through the Adept platform.

**Sources:** The design above is informed by Adept’s architecture plans and industry best practices for distributed work queues and messaging systems. It aligns with the Adept Framework’s vision of multi-channel messaging with minimal ops friction, ensuring that both internal developers and integration partners can trust the messaging subsystem to deliver notifications effectively and safely.
