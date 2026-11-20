# Proposal

## Why

Teams want to run practice and pickup rounds outside official competition days. Today every step of a round needs a volunteer: deploying pods, sending the packet, starting scoring, resetting boxes, tearing down. Paid self-service sessions ($40 for 4 hours, $80 for 8 hours) let teams run a round on their own and pay for the infrastructure they use.

## What Changes

- **A new app, practice.** Bookings, payment, environments, staff pages and the running of sessions live in their own application, apart from the portal and Battleship. It holds only the secrets sessions need.
- **Booking and payment.** A host picks a date and a 4-hour or 8-hour window and pays. No account is needed: the host manages the booking through a secret link emailed after payment. A booking can be cancelled for a full refund until anyone signs in, and is refunded if it is never started.
- **One session at a time.** At most one booking holds any time span. Sessions are refused from 7 days before an official event until that event is closed out and staff confirm its pods are torn down. Event dates are read from the portal.
- **Pickup sub-teams.** The host adds up to 8 sub-teams by email address. Each sub-team gets its own pod and its own emailed link.
- **Dedicated session pods and accounts.** Sessions use a pod range and team accounts that official events never use, so a session cannot touch an official team's account, password, pod or scores.
- **Environments.** Everything a session needs is one bundle, an environment: the VMs staff build in a Battleship environment, the team packet, the logins for the VMs, the scoring configuration, name resolution settings and an inject schedule per window length. Staff publish it as a whole; only published environments can be booked, and sessions get it exactly as published.
- **Start.** When the host presses Start inside the window, the pods are deployed, a scoring instance is created with a fresh database, and the session accounts are switched on with new passwords. Once every pod's services answer, the clock starts and each pod sees its sign-in, the VM logins and the packet. A start that fails for any pod is undone and refunded.
- **Scoring and mock injects.** Each session gets its own Quotient scoring instance. Injects open on the environment's schedule, counted from the session's start. The official scoring engine is not used.
- **Reset and power for teams.** A team can power its own pod's VMs on and off and roll them back to the baseline, with no staff involved. It can do nothing to any other pod.
- **End.** At the end of the window access is cut, the pods are torn down, the scoring instance is wiped, and results are sent to each pod.
- **Customers and staff.** A new customer's first booking is approved by staff before payment, and everyone accepts terms of use. Staff can see every booking, end or refund a session, block a customer, close dates and run unpaid sessions to try an environment.

Teams do not sign in to the portal, Battleship or the practice app. No staff are needed while a session runs.

The optional AI add-on (AI inject grading and a scripted red team) is a separate change, `practice-ai-addon`, built after this one.

## Capabilities

### New Capabilities
- `play-session-booking`: Pricing, window selection, the one-session-at-a-time rule, the official-event blackout, payment confirmation, refunds, terms of use, and the host's emailed link.
- `pickup-team-registration`: The host's pod plus up to 8 sub-team pods, sub-team invitation by email, a link per pod, and the session accounts' life and limits.
- `environment-lifecycle`: What an environment holds, publishing it, Start, pod deployment, release of sign-ins, VM logins and packet, name resolution, network isolation of session pods, and the ordered end of a session.
- `session-scoring`: A scoring instance per session with a fresh database, the environment's service checks and injects, a start that waits for the pods to answer, per-session team sign-in, the scoreboard a booking shares, and results after the session.
- `team-pod-controls`: A team's power and baseline-reset rights over its own pod's VMs, and the limits on them.
- `session-staff-controls`: What staff can see and do: approve new customers, end and refund sessions, block customers and dates, run unpaid sessions, and be told when a person is needed.

### Modified Capabilities

## Impact

- **`wccomps/practice` (new)**: a Go application, one program serving the web pages and running the worker, with its own database, pipeline, deployment and threat model. It reuses Battleship's sign-in, secret handling and page security by copying them. New outside service: Stripe.
- **`wccomps/battleship`**: an approval tag on template sets, and a narrow machine interface that accepts deploy and teardown jobs for the session pod range only. This is Battleship's first route that is not a signed-in volunteer page, so `THREAT_MODEL.md` and `ARCHITECTURE.md` change with it.
- **`wccomps/wccomps-portal`**: one read-only endpoint listing official event dates. Nothing else changes.
- **Quotient and aweful-dns**: a practice scoring instance, with name resolution, rebuilt from an environment for each session. No change to the official instance.
- **Proxmox and Authentik (configuration, not code)**: pools, networks and accounts for the session pod range; a role that lets a session account power and roll back VMs in its own pool; a session principal that can clone approved templates into the session pools; an Authentik token limited to the session accounts.
- **Network**: session pods must be cut off from official pods, from each other and from the management network, with the org's proxy as their only way to the internet. Nothing official teams use may route into them. See `threat-model.md`.
