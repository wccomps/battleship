# Tasks

## 1. Confirm assumptions and set up infrastructure

- [ ] 1.1 Check the items marked "to test" under "Assumptions" in `design.md` as the tasks that name them are done; record the result beside each, and stop to revise the design if one is false
- [ ] 1.2 Create Proxmox pools and team networks for the session pod range; verify a volunteer can deploy a set to pod 51 in Battleship and tear it down
- [ ] 1.3 Create the nine session accounts in Authentik, each in a group named for its own session team and in no official team group; verify signing in to the portal with one shows no team page, and that Battleship and every other Authentik application the org runs refuse it
- [ ] 1.4 Create the session role (`VM.Audit`, `VM.Console`, `VM.PowerMgmt`, `VM.Snapshot.Rollback`) and bind each session account to its own pool; verify by hand that `team51` can power and roll back a VM in pod 51 and is refused on pod 52 and when taking a snapshot
- [ ] 1.5 Create the session principal with the rights listed in `design.md` decision 5; verify that a deploy of an approved set to pod 51 run as the principal succeeds, adding to decision 5 any right it turned out to need, that Battleship's session interface lists the approved set for it (which needs `Sys.Audit` on `/`), and that it is refused a clone of an unapproved template, any action on an official pod, and a volume delete on the storage
- [ ] 1.6 Register the approval tag in Proxmox, spelled as in Battleship's config; verify a user without `Sys.Modify` on `/` cannot set it, nor the same tag in another case, and that a template carrying it can still be cloned
- [ ] 1.7 Apply network rules for session pods; verify from a VM in pod 51 that pod 52, official pods, the management network, the org's applications and a direct internet address are all unreachable, that a web request through the org's proxy works and the proxy's log names pod 51, and that pod 51 cannot be reached through the proxy or name resolution official teams use or from the official scoring host
- [ ] 1.8 Create the Authentik application for the practice scoring instance, open to the session accounts only; verify `team51` signs in and lands on team 51, and an official team account lands on no team
- [ ] 1.9 Open the org's Stripe account and create test-mode keys and a webhook secret; verify a test-mode checkout completes and its event reaches a test endpoint
- [ ] 1.10 Find out how the jump box is built and limit each session account to its own pod's networks, with name lookups through the session's aweful-dns; verify as `team51` that an SSH connection to a pod 51 VM works, one to pod 52 fails, and a pod 51 name resolves
- [ ] 1.11 Get the terms of use written and the VM images' software licences checked for use by paying customers; verify by the terms text being delivered and a written answer on licensing
- [ ] 1.12 Create the Authentik application for practice's staff sign-in, and a service account whose role holds only "reset password" and "view" on the nine session accounts; verify its token can set a session account's password and read its last sign-in, is refused adding that account to a group, enabling or disabling it, and any action on an official team account, a staff account, a group or an application binding, and that a password change ends the account's open Authentik sign-ins. Do this before any other practice work: `design.md` decision 11 names the fallback if it fails

## 2. Battleship: approval and the session interface

- [x] 2.1 Add `deploy.approved_tag` and report per set whether every master carries it; verify with tests in `internal/pods` covering all, some and none tagged
- [x] 2.2 Add session settings (pod range, principal user, key, listener; the `[practice]` section) that leave the interface off when unset; verify with config tests that a config without them loads with the interface off, and that each bad value is refused with a clear error
- [x] 2.3 Serve the session interface on its own listener: list approved sets, list a session pod's VMs, submit deploy or teardown, read and cancel its own jobs; record an example request and answer for each call as files practice's client is tested against; verify with tests that each call works with the principal's ticket and the key, and that no interface route is served when the settings are unset
- [x] 2.4 Refuse, without contacting Proxmox for a change: a ticket of any other user, a missing or wrong key, a pod outside the range, a kind other than deploy or teardown, a deploy of an unapproved set, and a read or cancel of a job the interface did not create; verify with one test per refusal
- [x] 2.5 Verify with a worker test that a set whose approval is removed after submission ends the job without deploying
- [x] 2.6 Update `THREAT_MODEL.md` (sections 1, 2, 3, 6 and 7) and `ARCHITECTURE.md` for the interface; verify `go test -race ./...` passes
- [ ] 2.7 Report each approved set's templates with their VMIDs and config digests; verify with a test that a template with another config reports another digest, and by rebuilding a template on the real cluster (the code and the test are done; the check on the real cluster is left)
- [x] 2.8 Report the memory, processors and disk one pod of each approved set takes; verify with a test against templates of known size
- [ ] 2.9 Let practice list the session interface's active jobs, so a submission whose answer was lost can still be found and cancelled; verify with a test that the list holds only jobs the interface submitted

## 3. The practice app: foundation, and the portal's one endpoint

- [ ] 3.1 Create the `practice` repository: a Go module building one program that serves the web pages and runs the worker, its own Postgres database with numbered migrations applied at startup, a pipeline copied from Battleship's (format, vet, race tests, vulnerability check, image publish), a minimal non-root image, and deployment manifests that start with bookings closed; verify the pipeline passes and a deployed instance answers its health check
- [ ] 3.2 Copy Battleship's sign-in, sessions, CSRF and sealing, remove the Proxmox sign-in step, and admit only accounts in the gold team or admin group; verify a session account and an official team account are both refused the staff pages, and that a staff member removed from the group loses access at the next session re-check
- [ ] 3.3 Write the Authentik client in Go with strict readers (set a password, read the last sign-in), copy the parts of Battleship's Proxmox client needed to log in with a password and to enable and disable users, and build the session account helper on both, limited to the session range; verify it refuses any number outside the range and that each reader has a property test
- [ ] 3.4 Copy Battleship's page security (no inline script, body limits with a larger one for environment uploads only, trusted proxies, a request log without query strings) and write stored files typed from their bytes; verify with a test per rule that a planted violation fails
- [ ] 3.5 Run the worker loop in the same program, taking an advisory lock on each pass and finishing its current step on shutdown; verify that two replicas running together carry out each due step once, and that a replica stopped mid-step leaves the booking in a state the other picks up
- [ ] 3.6 Write practice's `THREAT_MODEL.md` from `threat-model.md` in this change, naming the code copied from Battleship, and list the copies in Battleship's; verify with a test that fails when a function or type the file cites no longer exists
- [ ] 3.7 In the portal, add the read-only official events endpoint behind a key, with its property-test row and threat model entry; verify it lists event dates and whether one is open, answers nothing without the key, and that the portal's coverage tests pass
- [ ] 3.8 Read the portal's events in the worker, keep the last answer for up to a day, then close bookings and Starts and alert staff; verify with tests for a fresh answer, a stale one within a day, and one older than a day

## 4. Bookings, payment, links and environments

- [ ] 4.1 Add models for environments, bookings, pods, links and results, with pricing, the one-session-at-a-time rule, windows on the hour in Pacific time up to 60 days ahead, and the hold limits per email address and per network, under one advisory lock; verify with property tests that no two bookings' spans ever overlap and the price depends only on the window, and with a test for each refusal
- [ ] 4.2 Implement the blackout from the portal's event dates and the staff "pods torn down" switch, and cancel-and-refund when an event comes to cover a confirmed booking; verify with tests for each scenario of "Official event blackout" and "Bookings displaced by an official event are refunded"
- [ ] 4.3 Implement Stripe Checkout and its webhook: verify the signature over the raw body, store each event's id once, compare amount and currency; verify with tests for a forged event, a wrong amount and a repeated event, and that the webhook is the only route exempt from the CSRF guard
- [ ] 4.4 Implement emailed links: digest stored, token exchanged from the query string for a session limited to one booking or pod, changes by POST only, replace and expire; verify with tests that an unknown, replaced or expired link answers like a missing page and that a GET changes nothing
- [ ] 4.5 Implement sub-team invite, change and remove, a link for every pod including the host's, a booking link that shows no sign-in details, and the cap on invitation emails; verify that a pod link reaches no other pod and cannot manage the booking, and that the cap stops further emails
- [ ] 4.6 Implement environment upload and publishing for staff: packet, VM logins, scoring configuration, name settings and inject bundles per window length, checked together, with each template's VMID and config digest recorded from the session interface; verify an incomplete or unpublished environment is never offered, and that an environment whose template digest changed is withdrawn, staff are alerted and a host who had chosen it is emailed
- [ ] 4.7 Show the session sign-in, the VM logins and the packet on the pod page only once setup completes, and send neither login by email; verify the page before and after setup, and the sent emails
- [ ] 4.8 Require the host to accept the terms before payment and each pod to accept them before its sign-in details are shown, recording both; verify that no payment starts and no sign-in details appear without acceptance
- [ ] 4.9 Refuse moving a paid booking's window, allow changing its environment until Start, and limit its pods to what fits the session budget for the chosen environment; verify with a test per scenario of "A booking's window cannot be moved" and "A booking's pods fit the session capacity"

## 5. Scoring instance

- [ ] 5.1 Run the practice Quotient with Docker Compose on its own VM, and build the agent on that VM that recreates its database and writes its configuration from an environment with a team entry per pod, sign-in through Authentik, a generated admin password and a new cookie key; verify a rebuilt instance shows no team, score or start time from the run before, and that the agent accepts only rebuild and wipe from the practice app
- [ ] 5.2 Run aweful-dns on the same VM and have the agent write its configuration from the environment's name settings at each rebuild; verify that from the scoring VM a pod 52 name resolves to pod 52's address, and that a check by name succeeds
- [ ] 5.3 Write the Quotient client in Go with strict readers and use it to script team identifiers, activation, inject import and start; verify an inject scheduled 30 minutes after start opens 30 minutes after the scripted start on two consecutive rebuilds, and that each reader has a property test
- [ ] 5.4 Export each team's scores and submissions and the final standings to the practice app, then wipe the instance; verify the pod page shows them after the instance is gone
- [ ] 5.5 Restrict the instance's check runners and aweful-dns to the session pod networks; verify a check aimed at an address outside them fails to connect
- [ ] 5.6 Read Quotient's preflight round and start the competition only when every pod passes it; verify that with one pod's service down no scored round or inject starts, and that it starts once the service answers
- [ ] 5.7 Configure the practice instance so each pod sees every pod's total and service status by name, and nothing else of another pod; verify signed in as `team51` that the scoreboard shows all pods and that pod 52's service detail and submissions are refused
- [ ] 5.8 Post a notice in scoring 30 and 5 minutes before the window ends; verify both appear at the right times in a short test session

## 6. Session lifecycle

- [ ] 6.1 Implement the Battleship interface client with strict readers; verify each reader has a property test, and that the client reads every example answer Battleship records in task 2.3
- [ ] 6.2 Implement Start as in `design.md` decision 9, all or nothing, including the wait for trial checks and the start limit; verify with tests that a failure at each step, for one pod or all, leaves no VM, no enabled account and a refund
- [ ] 6.3 Implement End in its fixed order with retries, the closed-bookings state, the staff alert and the 30- and 5-minute notices on pod pages; verify with tests that a failing teardown keeps bookings closed and alerts, and that accounts are disabled before teardown is submitted
- [ ] 6.4 Implement host cancel: refund before Start; after Start, end the session and refund only when Authentik reports no sign-in by any of the booking's accounts since Start; no automatic refund and a staff alert when Authentik cannot answer; verify with one test per scenario of "The host can cancel for a full refund until someone signs in"
- [ ] 6.5 Close and refund a confirmed booking whose window ends unstarted; verify with a test that the refund is made once and the host is emailed
- [ ] 6.6 Record every booking transition and session action with the booking, the pod and who did it; verify each transition writes one row
- [ ] 6.7 Run a full session as staff against the real systems: create a staff booking, invite a sub-team, start, sign in once to console and scoring with the same account, reset and power a VM from the console, submit an inject, end; then repeat once as a customer paying in Stripe's test mode; verify afterwards that no VM, enabled account, live Proxmox ticket or scoring data remains

## 7. Staff controls and customer rules

- [ ] 7.1 Add the staff bookings page showing every booking with its state and history; verify only the gold team and admin groups reach it
- [ ] 7.2 Hold a first booking from an unapproved address until staff approve or refuse it, lapse it after the approval period, and send approved addresses straight to payment; verify with one test per scenario of "A new customer's first booking needs staff approval before payment"
- [ ] 7.3 Let staff end any session now and refund any paid booking, each with a recorded reason, and alert staff when a running session loses its pods or scoring; verify that a staff end follows End's order, that a refund is made once, and that no refund happens without a staff decision
- [ ] 7.4 Let staff block an address and close a span of dates, cancelling and refunding confirmed bookings inside a closed span; verify with tests that a blocked address is refused before payment and that closing dates refunds and emails the host
- [ ] 7.5 Let staff create an unpaid booking, including for a complete but unpublished environment, under every other booking rule; verify it can be started and that it is refused over a customer's window
- [ ] 7.6 Post each notice listed in "Staff are told when a person is needed" to the ops channel by webhook; verify each case sends one notice naming the booking, and that a failed send is retried and shown on the staff page
