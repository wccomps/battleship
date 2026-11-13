# Design

## Context

See `proposal.md` for motivation. Four existing systems already run a round, with a volunteer driving each step:

- **Battleship** (`wccomps/battleship`) deploys, resets and tears down pods on Proxmox. Every route needs a signed-in volunteer, every Proxmox call uses that volunteer's own login ticket, and it refuses to hold an API token (`removedKeys`, `noServiceToken`). Jobs on one pod run in order under the `team:<NN>` lock key (`Plan.LockKeys`). It accepts pods `00` to `99`.
- **The portal** (`wccomps/wccomps-portal`) holds the Authentik admin token and manages the `teamXX` accounts for teams 1 to 50 (`_team_user` refuses anything else). It assumes one open event: `one_open_event`, a single `CompetitionConfig`, and a global start and stop that toggles every team account.
- **Quotient** scores services and runs injects. One instance runs one event from one configuration directory. It records the first competition start in its database and schedules imported injects from it; that time survives restarts.
- **The Proxmox console** is where teams reach their VMs, signed in with the shared `teamXX` login (`docs/blue-team-cheat-sheet.md` in the portal). **The jump box** is a container teams reach over SSH with the same login, to test their services from outside the pod (`wccomps/competitor-container`).

Teams use the portal today only for packets, tickets and incident reports. A practice session needs none of those.

## Goals / Non-Goals

**Goals:**
- A session runs start to finish with no volunteer present.
- Nothing a session does can change an official team's account, password, pod, scores or the portal's event state.
- The existing systems keep their rules. Battleship still holds no Proxmox credential. The portal gains one read-only endpoint and nothing else.
- A break-in at the new app yields session secrets only, and a break-in at the portal yields none of them.

**Non-Goals:**
- More than one session at a time.
- Teams signing in to the portal, to Battleship or to the practice app.
- Tickets, staff support, orange team or red team during a session.
- AI grading and the AI red team. They are the `practice-ai-addon` change, built after this one.
- Any change to the official scoring engine or to how official events run.

## Decisions

### 1. One session at a time

One booking holds any time span, including a teardown gap after it.

Why: Battleship's capacity check is advice only and never blocks (`pods.Capacity`), so concurrent paid sessions could overcommit the cluster. One session also needs only one scoring instance with one fixed hostname.

Alternative: concurrent sessions on disjoint pods. It needs a scoring instance per session with its own hostname. Nothing here prevents adding it later.

### 2. Dedicated session pods and accounts

Sessions use pods `51` to `59` (configurable) and accounts `team51` to `team59`. Official events keep 1 to 50.

Why, from the portal's code:
- An official stop disables accounts 1 to 50 (`run_competition`). It never touches 51 and up.
- A session-end password reset on an official account would overwrite the password already mailed to a school (`SchoolInfo.password`).
- An enabled official team account can file a ticket at any time: `create_ticket` checks no event state. A ticket is live data (`live_competition_data_exists`), and live data with no open event blocks the next school-list import (`import_problem`) until staff run Clear leftover data. A session on pods 1 to 9 would let a paying customer block the next event's setup.
- The session accounts belong to no group that a portal `Team` row names, so they hold no portal permission (`groups_grant`, `team_for_groups`).

The Proxmox access list can then name the session pools alone, so the session principal (decision 5) has no rights on official pods.

The practice app manages the session accounts. The portal's `_team_user` keeps its 1 to 50 rule and the portal never touches them.

One-time setup by an admin: Proxmox pools and team networks for 51 to 59, nine Authentik accounts, and whatever the console and jump box need to route them.

Alternative: share pods 1 to 9 and rely on the blackout. Rejected for the ticket finding above.

### 3. A separate app, practice

Bookings, payment, emailed links, environments, staff pages and the Start and End sequences live in a new application, `practice`: its own repository, image, Postgres database and deployment, at its own address. It is written in Go, so the hardened parts of Battleship can be reused.

Why not a module in the portal, which an earlier draft of this design chose:
- **The reason for it went away.** That draft wanted to call the portal's import, start and close-out functions in-process. Sessions now have their own pods, accounts and scoring and use none of them.
- **Secrets.** In the portal, a public payment endpoint would run beside the Authentik admin token, which can reset any official team's password. And the portal, which competitors probe at every event, would gain Stripe keys and a Proxmox password that can clone and delete.
- **Event days.** Session code, migrations and bugs cannot take the portal down during a competition, and each releases on its own schedule.
- **Size.** Five capabilities, its own worker and its own staff pages are an application.

What practice holds: Stripe keys, the session principal's Proxmox password, the Battleship interface key, the scoring agent key, an Authentik token limited to the session accounts (decision 11), mail credentials, the ops channel webhook, and the key for the portal's events endpoint.

What it does not hold: the portal's Authentik admin token, the Discord bot token, the portal's Proxmox token, the official Quotient's admin sign-in.

Copied from Battleship, into practice's own repository:
- sign-in through Authentik with sessions that are re-checked, CSRF, and cookies only the app can open (`internal/auth`), with the Proxmox sign-in step removed and a group check added (decision 11);
- sealing of secrets and hashing of tokens (`internal/seal`);
- the Proxmox client's login, failover and certificate checks (`internal/proxmox`), with password login and user enable and disable added;
- page security: no inline script, body limits, trusted proxies, a request log without query strings (`internal/web/middleware.go`);
- startup checks and clean shutdown (`cmd/battleship/serve.go`), the migration runner, the pipeline and the deployment manifests.

Written new in Go, with the portal's versions as the reference: the Authentik admin client (`core/authentik_manager.py`, 394 lines), the Quotient client (`quotient/client.py`, 447 lines) and stored files typed from their bytes (`core/stored_files.py`, 146 lines). Also new: Stripe through its official Go library, mail, and the booking and staff pages.

No web framework. Battleship is built on Go's standard library with seven direct dependencies, and the copied code is written against it; a framework with its own request types would mean rewriting what was copied, and adds a large dependency to an app that holds a Proxmox password.

Cost of copying: a security fix to Battleship's sign-in has to be carried over by hand. Both threat models list the copies.

Alternatives:
- *Django, copying from the portal.* Faster for forms and staff pages. Rejected: the portal has nothing to copy for Proxmox tickets, sealed secrets or a restart-safe worker, its page security is weaker (inline script is allowed), and what it does offer is about 1,000 lines of plain HTTP clients.
- *A second program inside Battleship's repository.* Nothing copied, nothing drifts. Rejected: it puts two products in one repository, and anyone working on practice would need write access to Battleship.
- *A shared library both import.* Cleanest in the long run. Not now: it means restructuring Battleship's security code first.

Why not Battleship: it would put a public payment page in the app that every signed-in volunteer's Proxmox ticket passes through, and that keeps an encrypted copy of a volunteer's ticket for as long as their job is queued or running.

The one link to the portal is the official event dates (decision 10).

### 4. Emailed links instead of accounts

The host gets a link for the booking. Every pod, the host's own included, gets a link of its own. No customer has an account.

- A link carries a 256-bit random token. Only its SHA-256 digest is stored.
- The token travels in the query string of an exchange address. Practice's request log records the path and never the query string, as Battleship's does (`logRequests`), so tokens are not logged. The exchange sets a session limited to that booking or pod and redirects to an address without the token.
- Opening a link changes nothing. Every change is a POST with a CSRF check, so a mail scanner or link preview that fetches the link cannot start a session or remove a sub-team.
- A pod link can view its own pod and the booking's scoreboard, and nothing else. The booking link manages the booking and shows no pod's sign-in details, so the host can pass the host pod's link to teammates without handing over control, and cannot sign in as a sub-team.
- Emails carry the link only. Sign-in details appear on the pod page after Start and after that pod has accepted the terms, not in mail.
- Invitation emails are capped per booking, so the invite form cannot be used to send mail to arbitrary addresses.
- Links expire at the end of the retention period (30 days after the session, configurable).

Alternative: personal Authentik accounts with self sign-up. Rejected: a new public way into Authentik, for no gain over the link.

### 5. Deploy and teardown through a narrow Battleship interface

Battleship gains a machine interface on its own listener, not on the public address:

| Call | Does |
|---|---|
| List approved sets | Sets whose masters all carry `deploy.approved_tag`, each with its templates' VMIDs and config digests (decision 7) and the size of one pod (decision 16) |
| List a session pod's VMs | For readiness and teardown checks |
| Submit a job | `deploy` or `teardown` only |
| Read or cancel a job | Only jobs this interface created |

Battleship checks, for every call:
- a shared key, then a Proxmox ticket of the configured session principal. Battleship proves the ticket by asking Proxmox to renew it as the principal, which Proxmox does only for a ticket it issued to that user. Any other user's ticket is refused, so a stolen volunteer ticket is useless here. A call without the key sends nothing to Proxmox.
- every pod is inside the configured session pod range.
- a deploy names an approved set. The job carries a flag (`approved_only`) the planner enforces, and the worker re-plans before running, so approval is read again at run time.
- a deploy has nothing blocked. One VM the principal may not create refuses the whole request, with the reasons.

These checks stand in for the preview, fingerprint and typed confirmation a volunteer gives. The job's ticket is sealed in `job_credentials` and deleted when the job ends, as today. Battleship still holds no credential: the key without a valid ticket does nothing.

As built: the settings are Battleship's `[practice]` section (`pods`, `principal`, `listen`, `key`), off when unset. The calls are `GET /v1/sets`, `GET /v1/pods/{pod}/vms`, `POST /v1/jobs`, `GET /v1/jobs/{id}` and `POST /v1/jobs/{id}/cancel`. Each job records where it came from, and the interface reads and cancels only jobs it submitted. A request is a kind, a pod range and a set name, and nothing else. `internal/web/testdata/practice` in Battleship holds one recorded request and answer per call, for practice's client to be tested against (task 6.1).

What "approved" means in code:
- The approval tag must be one of Proxmox's registered tags, spelled exactly as in Battleship's config. If it is not, then under Proxmox's default tag setting anyone who may edit a master's options could add it, so Battleship lists no set and refuses every session deploy. The session principal needs `Sys.Audit` on `/` to see the registered tags; without it the result is the same.
- A deploy takes exactly the named set's masters. A set whose name is the ending of another's (`alpha` and `kilo.alpha`) is its own set.
- A session deploy clones templates a volunteer has built and never builds one. A host with no built template blocks the deploy.

`GET /v1/sets` plans a deploy of each approved set, so it lists exactly the templates a deploy would clone: per set its hosts, its built templates (host, name, VMID and a digest), the templates a deploy could not use with the planner's reason, and one pod's memory, processors and boot disks, summed over the built templates as Proxmox lists them. The digest is Battleship's hash of the template's config without its lock, tags and notes, so a note or a backup does not change it. A rebuilt template is a new clone with new identifiers in its config, so its digest should differ; that is read from how Proxmox clones, not yet seen on the org's cluster (task 2.7). A set or template that cannot be read, as when its node is down, is listed with the error, and the other sets are still answered.

The interface has no call that lists its jobs. If practice loses the answer to a submission, it cannot find that job (task 2.9).

Practice holds the session principal's Proxmox password and logs in for each job. This is the price of unattended work, since Battleship takes tickets only. The principal's rights, from `itemNeeds` and `templateNeeds` in `internal/pods/access.go`:

- `VM.Clone` on each approved template, granted per template. Not on the images pool, which also holds templates for events not yet run.
- `VM.Audit` on approved masters, so the planner can match them. No `VM.Clone` on masters: a volunteer builds each template once, and a missing or stale template blocks the deploy.
- `VM.Allocate`, `VM.Config.Network`, `VM.Config.Disk`, `VM.Config.CDROM`, `VM.Config.Options`, `VM.Config.Cloudinit`, `VM.Snapshot`, `VM.PowerMgmt` on the session pools.
- `SDN.Use` on the session pods' networks.
- `Datastore.AllocateSpace` on `deploy.storage`.
- `Sys.Audit` on `/`. Proxmox shows the registered tags to nobody without it, and Battleship approves nothing it cannot see registered. It is read-only, but it is over the whole cluster.
- The right to enable and disable the nine session users in Proxmox (decision 6).

Not granted: `Datastore.Allocate`, which would let it delete any volume on the storage. Freeing leftover disks (`StepFreeDisks`) therefore stays blocked for session jobs; staff clear leftovers. Not granted: `Sys.Modify`.

This list is read from the code, not proven. A template keeps its master's network cards, which sit on dev-slot networks, and Battleship's own note says Proxmox checks network rights on a clone's starting cards. A template with a custom processor model also needs `Mapping.Use` on it. Task 1.5 runs a real deploy as the principal and adds whatever is missing here.

Alternative: deploy with the host's own login. Rejected: it gives a shared competitor account the right to clone and delete.

### 6. Teams reset and power with their own Proxmox login

Each session account holds a role on its own pool only, with `VM.Audit`, `VM.Console`, `VM.PowerMgmt` and `VM.Snapshot.Rollback`. Teams use the Proxmox console they already use. Proxmox enforces the pod boundary.

- No `VM.Snapshot`, so a team cannot take a snapshot. The only one that exists is the baseline from deployment.
- No new code path for reset or power, and no competitor ever signs in to Battleship, whose job log is readable by everyone signed in and holds lab logins.

One thing this needs at the end of a session: a Proxmox ticket can be renewed with itself, without Authentik being asked again (`RenewTicket`), and Battleship's own threat model says removing someone in Authentik is not instant and to disable them in Proxmox to stop them at once. So End disables the booking's nine users in Proxmox itself, and Start enables them. Without this, someone from an earlier session could keep a ticket alive and reach the next customer's pod.

Alternative: buttons on the pod page that submit Battleship jobs. It widens the machine interface to reset and power and still leaves teams with console tickets, so it does not remove the step above.

Alternative: teams use Battleship instead of the Proxmox interface. It has two forms.

*Teams keep a Proxmox login and sign in to Battleship as volunteers do.* Battleship holds nothing new: each request uses the person's own ticket from their browser cookie, and Proxmox still enforces the pod boundary. Battleship's sign-in never sends the browser to Proxmox (`proxmoxStart`, `proxmoxCallback`), and the ticket sits in a cookie only Battleship can open (`sealTicketCookie`), so customers would not need to reach Proxmox at all and could not keep a ticket alive outside Battleship. That removes the End step above. Reset and power already exist there. What stands in the way:
- The job log shows every job to everyone signed in, lab logins included (§8). Customers would have to be limited to their own pod's jobs, and Battleship has no notion of roles to hang that on.
- Its threat model treats signed-in users as trusted volunteers.
- It has no console, VNC or websocket code. A console relay would be new, and the largest piece of work in the change.
- Team resets and power would be jobs in the queue volunteers share.

*Teams hold no Proxmox login and Battleship acts for them.* Battleship would have to keep the session principal's ticket for the whole session and enforce the pod boundary itself, reversing §2 and §7. Not recommended.

Decided: teams sign in with their Authentik session account and use the Proxmox console directly. The first form above remains the better end state if a console relay is ever worth building.

### 7. An environment is the one published bundle

Battleship already calls the place staff build a set an environment: VMs named `<host>.<domain>` in a dev slot, each tagged with the master tag and the domain, so the domain is the set (`kilo.alpha`). This change makes that same name the unit teams book. An environment is the Battleship environment plus everything else a session needs, published together by staff in the practice app:

- the team packet;
- the VM logins teams use on its boxes, entered at publishing. Not read from the masters' notes, which hold staff logins and which deploys clear from pod VMs;
- the Quotient configuration: boxes and checks, credential lists, custom check scripts;
- the name resolution settings for aweful-dns (decision 8);
- an inject bundle per window length (4-hour and 8-hour). An environment with no 4-hour bundle is not offered for half play.

The files are stored in the practice app, keyed by the domain. Battleship keeps no files beyond what Proxmox holds.

An environment can be booked only when three things hold:

1. Every master in its set carries `deploy.approved_tag`. The tag is listed in Proxmox's registered tags, so only someone with `Sys.Modify` on `/` can set it (`TagAccess`).
2. Staff have published it in the practice app.
3. The session principal holds `VM.Clone` on each of its templates. This is the only one of the three that Proxmox enforces.

**Published version, frozen.** A deploy reuses a template that exists and rebuilds it only when asked (`TemplateSpec.Rebuild`); the session principal cannot rebuild (decision 5). So sessions always get the templates as built, never a half-finished edit to a master. Publishing records each template's VMID and config digest, read through the session interface. The offer list and Start compare them: a template missing or replaced since publishing makes the environment unbookable and alerts staff. To update an environment, staff rebuild its templates with a volunteer deploy, then publish again. Its masters must stay where they are: the planner finds templates through them, so tearing down the Battleship environment makes it unbookable.

An environment that has been sold is public from then on and must not be used for a scored event.

### 8. A scoring instance rebuilt for each session

One practice Quotient instance, at a fixed hostname, runs with Docker Compose on its own VM, as Quotient ships. That keeps traffic to and from customers' VMs off the cluster that runs the org's applications. A small agent on the VM, also a single Go program, accepts two calls from the practice app, rebuild and wipe, and nothing else. At each Start it rebuilds the instance:

1. Drop and recreate its database. This is what resets the inject clock.
2. Write its configuration from the environment: a `[[team]]` entry for each pod of the booking with a long random password nobody is shown, sign-in through Authentik with the session account groups as its team groups, a generated admin password, and a new cookie key so no earlier session's cookie is valid.
3. Bring it up paused, set each team's identifier, mark the teams active, import the inject bundle for the window length.
4. Start it when the pods are ready (decision 15).

At End the practice app exports each team's scores and submissions and the final standings, then has the agent stop the instance and drop its database.

One sign-in for everything: teams use the same session account for the console, the jump box and scoring. This is how official events work. The price is one more thing that depends on Authentik, and one more one-time setup: an Authentik application for the practice instance, open to the session accounts only, with each session account in a group named for its team. The agent's rights stop at the practice instance. The instance's check runners can reach the session pod networks and nothing else.

Scoreboard: the instance is configured to show teams the graphs with team names (`DisableGraphsForBlueTeam = false`, `AllowNonAnonymizedGraphs = true`), so pods in a booking see each other's totals and service status. Service detail and submissions stay per team, as Quotient already enforces.

Name resolution: the same VM runs aweful-dns, as Quotient recommends and official events use. At each rebuild the agent writes its configuration from the environment's name settings (team domain, the team DNS server's last octet, internal and external address masks) for the booking's pods, so names like `team52.example.com` reach pod 52's DNS server and answers come back with pod 52's addresses. aweful-dns assumes 1 to 99 teams on `/24` networks, which covers pods 51 to 59.

Alternative: a new instance with a new hostname per session. It needs rights to create workloads, names and certificates at run time. With one session at a time, rebuilding one instance gives the same clean start.

### 9. Start and End sequences

States: `awaiting_approval`, `pending_payment`, `confirmed`, `starting`, `running`, `ending`, `ended`, `cancelled`, `failed_start`, `unused`. Transitions take one Postgres advisory lock per booking, so two presses of Start do one start.

Start, inside the window:
1. Check bookings are open (decision 10), and that the environment is still published, its VMs all approved, and its templates the ones recorded at publishing.
2. Set new Authentik passwords for the booking's session accounts and enable them in Proxmox.
3. Submit the Battleship deploy for the booking's pods.
4. Rebuild the scoring instance.
5. When the deploy has finished, wait for every pod to pass scoring's trial checks (decision 15).
6. Start scoring, show the session sign-in, the environment's VM logins and the packet on the pod pages, email each pod a notice without them.

If any step fails for any pod: run End's steps for whatever exists, refund, mark `failed_start`.

Cancel, by the host: before Start it frees the window and refunds. After Start it runs End, and refunds only if no session account of the booking has signed in since Start gave it a new password. Every sign-in goes through Authentik (decision 8), so practice asks Authentik for each account's last sign-in. If Authentik cannot answer, End still runs, nothing is refunded automatically, and staff are alerted.

End, at the window's end, when the host ends it, or when staff do:
1. Stop scoring.
2. Disable the session accounts in Proxmox, and replace their Authentik passwords with random ones, which ends their Authentik sign-ins (decision 11).
3. Cancel the booking's pending Battleship jobs, then submit the teardown with a fresh ticket.
4. Check the pods hold no VMs.
5. Export results, wipe the scoring instance.
6. Email each pod.

Thirty and five minutes before the window ends, the worker posts a notice in scoring and on the pod pages. A confirmed booking whose window ends with Start never pressed is closed and refunded.

The worker (decision 14) drives due transitions and retries. While any End step is incomplete, bookings are closed and staff are alerted; the worker keeps retrying.

### 10. Blackout from the portal's event dates, and the bookings switch

The portal gains one read-only endpoint that lists official events: each one's date, and whether an event is open now. It is protected by a key and answers nothing else. This is the only change to the portal.

Practice reads it every few minutes and before each booking and Start. Bookings close by themselves when an official event is open or dated within 7 days. A newly created or moved event is noticed at the next read, and confirmed bookings it now covers are cancelled and refunded.

If the portal cannot be read, practice uses its last answer for up to a day. After that it refuses new bookings and Starts and alerts staff: an event it does not know about is worse than a closed calendar.

After an event, bookings reopen only when no event is open or near and staff press a "pods torn down" confirmation in practice. Practice cannot see official pods without giving the session principal rights on them, which decision 2 exists to avoid. A person confirming is the simple answer.

The same moment is when an admin disables the session principal in Proxmox for the event, and enables it again afterwards. A booking rule does not bind a compromised app; a disabled Proxmox user does.

Alternative: staff enter event dates in practice by hand. Rejected: a forgotten entry puts a paid session on top of an event.

### 11. Staff sign-in and the app's Authentik token

Staff sign in to practice through Authentik, with an Authentik application of its own. The staff pages need the gold team or admin group (`WCComps_GoldTeam`, `WCComps_Discord_Admin`), the same groups the portal's `PERMISSION_MAP` names. Battleship's sign-in asks Authentik for no groups and has no roles, so the group check is new code on top of the copied sign-in. Nobody else has anything to sign in to: customers use links.

Practice's Authentik token belongs to a service account whose role holds two permissions, each on the nine session accounts and on nothing else: reset password, and view. Authentik grants permissions on single objects, and its set-password call checks the permission against the user it is asked about. The token cannot touch an official team account, a staff account, a group or an application binding.

It is not given the right to change a user. In Authentik that right includes putting the user into any group that is not a superuser group. With it, a break-in at practice could add `team51` to the gold team group, set its password, and sign in to the portal as gold team. So practice never enables or disables an account in Authentik:

- Between sessions a session account has a random password nobody knows. Start sets the session's password. End sets another random one.
- A password change ends that account's open Authentik sign-ins. Authentik uses Django's session check, which drops sessions made under an old password; its own code spares only the session of whoever made the change. To confirm in task 1.12.
- The Proxmox users are still disabled at End (decision 6). That is what cuts the console and any ticket kept alive.
- The last sign-in is read with the view permission.

Fallback, if task 1.12 shows the token cannot be held this narrow or a password change leaves sign-ins open: session accounts that live in Proxmox and Quotient instead of Authentik, with one generated password set in both by practice. Practice would then hold no Authentik token at all. The costs: session teams sign in differently from official events, the jump box needs its own answer, and the refund rule needs another way to tell that someone signed in.

### 12. Payment through Stripe; internet through the org's proxy

Payment: Stripe Checkout, in an account the org opens. Practice creates a checkout session with its own price, the booking's reference and an expiry equal to the hold period, and confirms the booking on Stripe's signed event. Refunds go through Stripe's API. Stripe keeps its fee on a refunded payment, so each refund costs the org about $1.50 to $3.

Considered: Square and Zoho Payments cost the same (2.9% + 30¢); PayPal costs more at these amounts (3.49% + 49¢). The GradRoots ticket link used for invitational fees today has no public API or payment notification that was found, so it cannot confirm a booking by itself.

Internet: every host in a session pod reaches the internet only through the proxy the org controls; everything else outbound is blocked. That makes the proxy the one place to see and limit what a rooted VM sends out. This change needs two things of it: requests recorded with the pod they came from, so abuse traces to a booking, and no change to what it already blocks.

### 13. The jump box limits each session account to its own pod

The jump box is shared. A session account's outbound connections from it are allowed only to its own pod's networks, and its name lookups go to the session's aweful-dns. One way: firewall rules on the jump box matched on the account's user ID, written once for the nine accounts, since each always maps to the same pod number.

How the jump box is built today was not checked. If it runs one container per team, a container per session pod, attached only to that pod's networks, does the same more simply.

### 14. A worker process for everything that happens on a clock

Practice is one program, as Battleship is: each replica serves the web pages and runs a worker loop. The worker carries what must not depend on a browser staying open: due transitions, polling the deploy and the trial checks, retries, the end-of-window warnings, the no-show refund, reading the portal's event dates. Each pass takes a Postgres advisory lock, so with two replicas only one acts. On shutdown a replica finishes the step it is in and starts no other.

### 15. The clock starts when the boxes answer

Quotient runs unscored preflight rounds before a competition starts, with the same checks as scored rounds, and reports the latest one (`GET /api/engine/preflight`). After the deploy finishes, the worker polls it. When every pod's checks pass, it starts the competition, which starts scored rounds and the inject clock. If a pod has not passed within the start limit (15 minutes, configurable), the start has failed.

Starts are all or nothing. A pod that fails to deploy or never passes preflight fails the whole start: End's steps run for whatever exists, the booking is refunded, staff are told.

### 16. A capacity budget instead of Battleship's advice

Battleship's capacity check never blocks. So the session interface reports, for each approved set, the memory, processors and disk one pod of it takes, summed from its templates. Staff set one session budget in practice. A booking may have as many pods as fit: budget divided by the environment's size, at most 9. The check runs when the host adds a sub-team or changes environment.

### 17. Customers, terms and staff controls

- **New customers are approved before they pay.** A first booking from an address holds its window and posts to staff; approval emails the host a link to pay. Approving before payment means a refusal costs no Stripe fee. Approved addresses skip this. It is the one step that needs a person, and it happens days before any session.
- **Terms.** The host accepts at checkout; each pod accepts on its link before sign-in details are shown. Both are recorded.
- **Staff pages** in practice (decision 11): all bookings with their history, approve or refuse, end now, refund, block an address, close dates, create an unpaid staff booking, publish environments, set the session budget.
- **Alerts** go to the ops channel by a Discord webhook that practice holds. They do not depend on the portal or its bot. The staff pages show the same state if Discord is down.
- **Times** are Pacific, on the hour, up to 60 days ahead.

## Risks / Trade-offs

- **A Proxmox password with clone and delete rights sits in an internet-facing app** → rights limited to the session pools and approved templates; ticket user pinned in Battleship; the principal disabled during official events; the app holds no official secret.
- **Another application to run** → its own repository, pipeline, database and threat model, for a volunteer team. Accepted for what decision 3 buys.
- **Code copied from Battleship drifts from the original** → both threat models name the copies; a fix to one is carried to the other. If that becomes a burden, the shared pieces move to a library both import.
- **Go gives no forms, admin pages or database layer for free** → booking forms and staff pages are written by hand, as Battleship's pages are. Accepted for the reuse in decision 3.
- **Rooted VMs run unattended for up to 8 hours** → no route to anything inside the org, and the internet only through the org's proxy, which logs by pod (decision 12). A rooted VM can still send web requests through the proxy.
- **End fails and VMs keep running** → accounts are cut before teardown; bookings close; staff are alerted.
- **A volunteer's "all teams" job in Battleship includes session pods** → sessions and event preparation are kept apart by the blackout; outside it, a volunteer could still tear down a running session by mistake. Accepted.
- **Leftover disks from session pods accumulate** → staff clear them; the principal is deliberately unable to.
- **One practice scoring hostname is reused** → new cookie key and admin password per session.

`threat-model.md` in this change is the full threat model.

## Migration Plan

1. Admin setup, no code: session pools, networks and accounts; the session role and principal; the registered approval tag; the practice scoring VM; network rules for session pods; Authentik applications for practice and the practice scoring; the Stripe account.
2. Battleship: approval tag, session settings, the machine interface. With no session settings the interface is off and nothing changes for volunteers.
3. Portal: the read-only events endpoint.
4. Practice: the new app, deployed with bookings closed.
5. Publish one environment, run a full session as staff, then open bookings.

Rollback: close bookings in practice and disable the session principal in Proxmox. Official events and the portal are untouched either way.

## Assumptions

Answered by the org:
- The console at `competitors.wccomps.org` is the standard Proxmox interface and teams sign in through Authentik, so a Proxmox role is all a team needs to power and roll back (decision 6).
- The org controls the Authentik bindings, so the console, jump box and practice scoring can be opened to a session group without opening them to official teams (decision 2).
- Every host's only way to the internet is a proxy the org controls; all other outbound access is blocked (decision 12).

Checked in source or documentation, not run:
- Proxmox checks that a user is enabled on every request (`auth_handler` in `PVE::HTTPServer`) and when a ticket is renewed (`authenticate_user` in `PVE::AccessControl`), so disabling a user ends its tickets (decision 6).
- Updating a Proxmox user needs `User.Modify` on a group the user is in (`update_user` in `PVE::API2::User`), so the session principal can be limited to the nine session users. The same right lets it change those users' other settings (decisions 5 and 6).
- Quotient's admin API has routes for team setup, inject import, competition start, pause, preflight, and score and submission export (`www/router.go`) (decisions 8 and 15).
- Authentik's API has a `last_login` field on users (decision 9).
- Authentik grants permissions on single objects, and its set-password call checks "reset password" against the user it is asked about (`permission_required` in `authentik/rbac/decorators.py`, `set_password` in `authentik/core/api/users.py`) (decision 11).
- In Authentik, whoever may change a user may add that user to any group that is not a superuser group (`validate_groups` in `authentik/core/api/users.py`). Practice's token is therefore given no right to change users (decision 11).

To test:
- Quotient's Authentik sign-in places each session account on its own team by group name and an official team account on none. Documented in its README; task 1.8.
- Session pods cannot reach each other, official pods or the management network, and reach the internet only through the proxy, which logs by pod. Task 1.7.
- The proxy can tell one pod from another by source address. Task 1.7.
- The proxy and name resolution official teams use have no route to pods 51 to 59. Task 1.7.
- Rebuilding a template changes its config digest, so a replaced template is noticed. Task 2.7.
- The session principal, holding `Sys.Audit` on `/`, is shown Proxmox's registered tags, and a user without `Sys.Modify` on `/` cannot put the approval tag, or the same tag in another case, on a VM. Tasks 1.5 and 1.6.
- How the jump box is built, and that a session account on it reaches only its own pod. Task 1.10.
- A deploy of an approved set succeeds with only the rights in decision 5. Templates keep their masters' network cards, on dev-slot networks, and Proxmox may ask for rights on those at clone time. Task 1.5.
- Battleship and every other org application refuse a session account. Task 1.3.
- A role holding only "reset password" and "view" on the nine session accounts can set their passwords and read their last sign-in, and is refused everything else; and a password change ends that account's open Authentik sign-ins. Task 1.12.

## Open Questions

None of these changes the approach.

- Disputed charges after a session has been played. Policy, not code.
- Length of the teardown gap, the hold period, the approval period, the start limit and the retention period.
- Whether Start may be pressed a short time before the window so pods are ready on the hour.
- The wording of the terms of use, and whether the VM images' software licences allow use by paying customers. Questions for the org, not for code.
- The practice app's public address.
