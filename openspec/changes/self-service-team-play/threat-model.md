# Threat Model: self-service-team-play

Scope: the six capability specs and `design.md` in this change. It extends `THREAT_MODEL.md` in `wccomps/battleship` and `wccomps/wccomps-portal`; "§" refers to Battleship's file. The practice app's own `THREAT_MODEL.md` is written from this one (task 3.6).

History: the first version modelled a design where the portal hosted every team page and sessions shared pods 1 to 50 with official events. The second moved sessions to their own pods, accounts and scoring but kept the code in the portal. This version moves it to a separate app and moves the AI add-on to the `practice-ai-addon` change. Section 7 says what happened to each earlier finding.

Status of each finding: **In spec** (a requirement or design decision covers it), **to test** (covered on paper, rests on an assumption listed in `design.md`), **Open** (not covered), **Accepted**.

## 1. What is new

| New thing | Why it matters |
|---|---|
| A paying member of the public | First actor who is neither a volunteer nor an invited school. Can dispute charges. |
| A new internet-facing app | It holds a Proxmox password, payment keys and an Authentik token, and takes input from strangers. |
| Pods deployed, used and destroyed with nobody present | Today each of those steps has a volunteer's own Proxmox login behind it. |
| A machine caller on Battleship | Every Battleship route today needs a person's session and ticket (§3), and Proxmox's access rules are the only gate (§7). |
| Competitors with power and rollback rights in Proxmox | Today a reset goes through ticket staff. |
| Links in email as the only credential for a booking or pod | No account stands behind them. |
| A scoring instance rebuilt by automation | Its runners connect to VMs a customer controls. |
| A payment provider | A public webhook that changes state. |
| One app trusting another's answer | Practice closes and cancels bookings on what the portal says about events. |

## 2. Attackers

| Attacker | Starts with | Wants |
|---|---|---|
| Anonymous visitor | The booking page, the webhook address | A free session; every window held and unpaid; a look at environments not yet released |
| Booking host | A paid booking, the booking link, one pod | Control of sub-team pods; more time; a refund after playing |
| Sub-team member | A pod link, that pod's sign-in, root on its VMs | Another pod's VMs, packet, submissions or results; access after the session; the next customer's pod |
| Anyone who gets hold of a link | A forwarded or mistyped email, a shared mailbox | Whatever that link's holder may do |
| Rooted pod VM | Whatever a competitor runs on it, and every reply it gives | Other pods, the cluster, the internet; control of the scoring runners |
| Whoever compromises the practice app | Its secrets and network position | Everything the session principal can do; environments not yet released; customers' details; a way on to the portal or Battleship |
| Whoever compromises the portal | Its secrets, and the events endpoint practice trusts | To cancel or block sessions; session secrets |
| Careless volunteer | Rights to tag masters and run Battleship jobs | Not hostile: approves a set that should stay private, or tears down "all teams" during a session |

## 3. Assets, worst loss first

1. **Environments for events not yet run**: masters, templates, and the official scoring engine's injects and credential lists.
2. **Official event state**: team accounts and their mailed passwords, scores, pods, and the portal's event stage.
3. **The portal's and Battleship's secrets**: the Authentik admin token, volunteers' Proxmox tickets. Practice must not be a way to them.
4. **The session principal's Proxmox password**, held by practice.
5. **The cluster's capacity and Battleship's job workers.**
6. **The org's network reputation**: what rooted VMs can send outward.
7. **One customer's session from the next**: pods, sign-ins, submissions, results.
8. **Payment integrity and customers' email addresses.**
9. **Practice scores.** They decide nothing.

## 4. Findings

### S-01 Sessions and official events share infrastructure — High, In spec

Dedicated pods 51 to 59 and separate accounts remove the shared names: an official stop does not touch them, a session reset cannot overwrite a school's mailed password, and a session account holds no portal permission. The last matters because an enabled official team account can file a ticket with no event open, and that blocks the next school-list import.

The blackout and the staff switch keep sessions away from event preparation. What remains shared is the cluster itself.

Residual, accepted: Battleship's "all teams" means every pod with VMs, so a volunteer's all-teams teardown outside the blackout would destroy a running session.

### S-02 A Proxmox password with clone and delete rights is stored in an internet-facing app — High, In spec, to test

Battleship takes login tickets only, so unattended work means a stored password. It sits in practice.

Controls: the rights in `design.md` decision 5, which name the session pools, the session networks and individual approved templates. No rights on official pods, the images pool, masters beyond read, or `Datastore.Allocate`. During official events an admin disables the user in Proxmox, which holds even if practice is compromised.

What a compromise of practice yields through it: destroy and redeploy session pods, clone approved templates, switch session users on and off. It cannot reach official pods or unapproved templates.

The list of rights is read from code. Task 1.5 runs a real deploy with it and proves both that it is enough and that the refusals hold.

### S-03 Battleship gains a route outside its two invariants — High, In spec

- Accepts only tickets of the configured session user. Without this a stolen volunteer ticket plus the key would run jobs with no Authentik session behind it. Battleship does not judge the ticket itself: it asks Proxmox to renew it as the session user, which Proxmox does only for a ticket it issued to that user.
- The key is checked first. A call without it sends nothing to Proxmox.
- Own listener, off the public address.
- No human confirm. In its place: deploy and teardown only, pods inside a configured range checked in Battleship, approved sets only, and a deploy refused whole if any VM in it is blocked.
- Reads and cancels only jobs it created. Each job records where it came from, and only the interface writes its own mark. Volunteers' jobs hold lab logins (§8).
- Session job inputs are a kind, a pod range and a set name. They carry no booking reference and no customer details, since every volunteer can read them.
- The key alone does nothing: Battleship still holds no credential.
- Approval is read as the session user sees the cluster. A master it cannot see is not counted, so it needs `VM.Audit` on whole sets, and practice records an environment's templates at publishing (S-04).
- Approval counts only if the tag is one of Proxmox's registered tags, in the exact spelling. Battleship checks this on every listing and every session deploy, and approves nothing otherwise. To see the registered tags the session user needs `Sys.Audit` on `/`, a read-only right over the whole cluster; that Proxmox refuses the same tag in another case is to test (task 1.6).
- A session deploy clones built templates and never builds one from a master, so the session user needs no clone right on any master.
- The interface's listener is plain HTTP. The key and the session user's ticket cross it unencrypted, so only practice may be able to reach it, over a network nobody else can read.

Battleship's `THREAT_MODEL.md` §1, §2, §3, §6 and §7 are updated in the same change (task 2.6).

### S-04 Releasing an environment to the public — High, In spec

Three keys: a registered approval tag that only `Sys.Modify` on `/` can set; an environment published by staff in practice; a per-template clone grant to the session principal. Only the third is enforced by Proxmox, and it is the one that matters if practice is compromised.

Approval and the published template digests are read again at Start, and approval by Battleship's worker. A sold environment is public and is not reused for a scored event; that is a rule for people, not code.

The official scoring engine is never used for a session, so its injects and credential lists are out of customers' reach.

### S-05 Rooted VMs with nobody watching — High, In spec, to test

`environment-lifecycle` requires that a session pod reaches no other pod, no official pod, and nothing of the cluster or the systems that run the org's applications. Its only way to the internet is the proxy the org controls; every other outbound connection is blocked. Task 1.7 tests each of these from inside a pod.

What remains: a rooted VM can send web requests through the proxy for up to eight hours. The proxy records them by pod, so abuse traces to a booking and an approved customer, and whatever the proxy already blocks stays blocked. What it allows today was not checked.

### S-06 The end of the window — High, In spec

Order: stop scoring, cut accounts, tear down, verify, wipe, report. Accounts go before teardown so a stuck teardown leaves VMs running but unreachable through the console and jump box. A session that cannot be cleaned up closes bookings and alerts staff. Staff can end any session at once by the same steps.

A shell opened from inside a VM lasts until the VM is deleted. Its only way out is the org's proxy (S-05).

### S-07 A session account outlives its session — High, In spec

A Proxmox ticket can be renewed with itself; Authentik is not asked again. Battleship relies on this for volunteers and its §8 says removal in Authentik is not instant. A member of Monday's session could keep a ticket alive and use it against Tuesday's customer on the same pod number, with power, rollback and console.

Control: End disables the booking's users in Proxmox itself, Start enables them. `pickup-team-registration` and `team-pod-controls` both require that an earlier sign-in is refused later.

Both parts were checked in Proxmox's source. It checks that a user is enabled on every request and on ticket renewal, so disabling ends the ticket. Updating a user needs `User.Modify` on a group the user is in, so the principal can be limited to the nine session users; the same right lets it change those users' other settings, which reach no further than their own pods. Task 6.7 checks for a live ticket after a real session.

The same property holds for official team accounts between events today. Out of scope here; worth telling whoever owns the official flow.

### S-08 A public payment webhook — Medium, In spec

Practice's one route that changes state with no CSRF token. The signature check over the raw body stands in for it, and it is the only exemption the CSRF guard allows (task 4.3).

Confirms only on a verified report with the right amount and currency, once per report id. The browser's return proves nothing. Pending bookings are limited per email address and per network, with a short hold.

In the earlier design this handler ran beside the portal's Authentik admin token. Now a bug in it reaches practice's secrets only (S-22).

Refunds until the first sign-in mean a host can start a session, let the pods deploy, and cancel for free. Each round costs the org Stripe's fee, which is not returned on a refund, and some cluster time. One session at a time and the teardown gap bound how often, and staff can block the address. Accepted.

Open: a customer who plays and then disputes the charge. Policy, not code.

### S-09 Links as credentials — Medium, In spec

A link is everything: whoever holds the booking link runs the booking; whoever holds a pod link sees that pod's sign-in.

- 256-bit tokens, stored as digests, kept out of logs by travelling in the query string and being exchanged for a session.
- Opening a link changes nothing, so mail scanners and previews are harmless.
- A pod link cannot manage. The booking link shows no pod's sign-in details, so the host passes teammates the host pod's link, not control of the booking.
- A new link goes only to the address on the booking and kills the old one.
- A mistyped sub-team address sends a stranger a link. The host can replace it until Start; after Start the pod is theirs for the session.

Accepted: no way to tell a forwarded link from the original. The same is true of the shared team accounts already in use.

### S-10 The scoring instance — Medium, In spec, to test

- Its runners connect to rooted VMs and read their replies; custom check scripts run against those replies. A runner that can reach only the session pod networks limits what a hostile reply can lead to.
- Rebuilt per session with a new database, cookie key and admin password, so nothing carries over at a reused hostname.
- Teams sign in with their session account through Authentik. Quotient places an account on a team by its group, so an official team account that reaches the practice instance lands on no team. Cutting the session account at End cuts scoring too.
- The agent can rebuild this one instance and nothing else, and takes two calls from practice.
- It runs on its own VM, off the cluster that runs the org's applications.

Quotient's admin API has the routes this needs (`www/router.go`), read but not exercised. Its Authentik sign-in mapping accounts to teams is documented and tested in task 1.8.

### S-11 What a team's Proxmox role allows — Medium, In spec

`VM.Audit`, `VM.Console`, `VM.PowerMgmt`, `VM.Snapshot.Rollback` on its own pool. No snapshot, no configuration, no allocate. Proxmox enforces the pod boundary, including against the host.

The Proxmox interface is already exposed to competitors at official events; this adds two privileges to what they hold there.

### S-12 and S-13 AI grading and the AI red team — moved

Both belong to the `practice-ai-addon` change and are covered in its design. Nothing in this change calls an AI provider or attacks a pod.

### S-14 Starving volunteers' work — Low, In spec

A session submits two Battleship jobs, deploy and teardown. Teams' resets do not go through Battleship, so nine pods cannot fill its job workers. Deploy shares the clone slots with any volunteer job running then; the blackout keeps that away from event preparation, and the capacity budget bounds how large a deploy can be.

### S-15 Audit — Low, In spec

Every booking transition and session action is recorded in practice with booking, pod and who did it. Practice keeps the Battleship job number beside the booking; the job itself names no booking. A pod's actions are attributable to the pod, not a person, as with shared team accounts today.

### S-16 The jump box and name resolution — Medium, In spec, to test

The jump box is shared with official teams and sits outside the pods, so it is a way into them. A session account on it may connect only to its own pod's networks; otherwise a sub-team could reach another pod in its booking from there, which the isolation requirement forbids. How the jump box is built was not checked, so how this is enforced is open (task 1.10).

aweful-dns runs on the scoring VM and forwards each pod's names to that pod's own DNS server, which a team controls. A hostile answer can point a check or a team's lookup at another address. Scoring and aweful-dns can reach only the session's pod networks (task 5.5), so a lying answer leads nowhere else.

### S-17 What an environment releases — Medium, In spec

- VM logins shown to teams are entered at publishing, not read from the masters' notes, which hold staff logins. Deploys already clear notes from pod VMs (`StepNotes`).
- Sessions get templates as built, never a master mid-edit: a deploy reuses existing templates and the session principal cannot rebuild them. Publishing records each template's config digest, so a template rebuilt without publishing again is noticed and withdrawn rather than sold unreviewed.
- Sign-ins and VM logins appear only on the pod page after Start, never in email.

### S-18 Who may book — High, In spec

Open booking would let anyone with a card, including a stolen one, buy eight hours of rooted VMs on the org's network, and each disputed charge costs the org $15.

- A first booking from an address needs staff approval before payment. Staff see who is asking before any VM exists, and a refusal costs no fee.
- Host and every pod accept terms of use that say what the VMs may be used for. Acceptance is recorded.
- Staff can end a session at once, refund, and block an address.
- Staff can close dates.

Residual: approval is a judgement on an email address and whatever the host writes. A known customer's link or mailbox can still be taken over (S-09).

### S-19 Session accounts and other applications — High, In spec, to test

A session account has an Authentik sign-in and a Proxmox login. Battleship asks for exactly those two things. If its Authentik application admitted session accounts, a customer could sign in and read the job log, which every signed-in user sees and which holds lab logins (§8). The same goes for any org application open to "anyone who can sign in", practice's own staff pages included.

`pickup-team-registration` requires that a session account signs in to the console, the jump box and its session's scoring only. Task 1.3 tests it against Battleship and every other application, and task 3.2 against practice's staff pages.

### S-20 Routes into a pod from shared services — High, In spec, to test

Official events run services that route into pods by team number: the proxy teams use, name resolution, the scoring engine. If any of them also covers pods 51 to 59, whoever can reach that service reaches a customer's pod, whatever the pod's own rules say. The spec forbids it and task 1.7 tests it from the outside in, not only from inside a pod.

### S-21 Invitation and booking forms as a mail and hold channel — Low, In spec

The invite form sends mail to addresses a host types. Invitation emails are capped per booking, and only an approved customer who has paid reaches the form. Unpaid holds are limited per address and per network.

### S-22 The practice app as a target — High, In spec, to test

It faces the internet, takes input from strangers, and holds: the session principal's Proxmox password, Stripe keys, the Battleship interface key, the scoring agent key, an Authentik token, mail credentials and customers' email addresses.

What limits a break-in:
- It holds none of the portal's or Battleship's secrets, and shares no process, database or image with either.
- Each secret it holds is narrow: the Proxmox password by its rights (S-02), the Battleship key by the interface's own checks (S-03), the agent key to rebuild and wipe.
- Its Authentik token holds two permissions, reset password and view, on the nine session accounts only. Authentik's source shows permissions can be granted on single users and that its set-password call checks them per user (task 1.12 confirms it on the org's Authentik).
- **The token must not be able to change a user.** In Authentik, changing a user includes adding it to any group that is not a superuser group. A token with that right, taken in a break-in, could put a session account in the gold team group, set its password, and sign in to the portal as gold team. Practice therefore never enables or disables accounts in Authentik; it sets passwords, and relies on a password change to end open sign-ins.
- Staff sign in through Authentik and need the gold team or admin group. No customer has an account.

Copied code: practice's sign-in, sessions, secret sealing, page security and Proxmox login are copies of Battleship's. A flaw fixed in one stays in the other until someone carries the fix across. Both threat models list the copies (task 3.6). Its Authentik and Quotient clients are new code, written from the portal's as a reference, and have not had the portal's years of use.

### S-23 Trusting the portal's event dates — Medium, In spec

Practice closes bookings, and cancels and refunds confirmed ones, on what the portal's events endpoint says.

- A portal that is down: practice uses its last answer for a day, then refuses new bookings and Starts and alerts staff. A closed calendar is the safe failure.
- A portal that lies, because it is compromised or the answer is forged: it can make practice cancel and refund every booking, or hide an event so sessions run during it. Every cancellation for an event is posted to staff with the event named. A hidden event still meets the staff "pods torn down" switch after the last real one, and the session principal disabled in Proxmox for the event's duration.
- The other direction: the endpoint is read-only, answers only event dates and whether one is open, and needs a key. Practice holds nothing else of the portal's.

Accepted: a compromised portal can cost the org a round of refund fees before staff notice.

## 5. Accepted

- Practice scores can be gamed.
- A pod's members cannot be told apart, and a link cannot be told from a forwarded copy.
- The host can end the session early for every pod.
- A sold environment is public.
- A compromised practice app can destroy and redeploy session pods.
- A compromised portal can cancel sessions.
- A volunteer can tear down a running session by mistake.

## 6. Open

| # | What | Who can close it |
|---|---|---|
| 1 | Session pod isolation and proxy-only internet, tested from inside a pod and from outside in (S-05, S-20) | Task 1.7 |
| 2 | Quotient's Authentik sign-in places session accounts on their own teams (S-10) | Task 1.8 |
| 3 | What the proxy allows, and whether that is acceptable for unattended rooted VMs (S-05) | Whoever runs the proxy |
| 4 | A customer who plays and then disputes the charge (S-08) | Product decision |
| 5 | How the jump box is built, and limiting session accounts to their own pod on it (S-16) | Task 1.10 |
| 6 | The session principal's rights are enough for a real deploy, and no wider (S-02) | Task 1.5 |
| 7 | Battleship and other org applications refuse session accounts (S-19) | Tasks 1.3 and 3.2 |
| 8 | Practice's Authentik role works as designed: two permissions on nine accounts, and a password change ends open sign-ins (S-22) | Task 1.12 |
| 9 | Terms of use, and software licences for paying customers (S-18) | The org |

## 7. What became of earlier findings

| Finding | Earlier | Now |
|---|---|---|
| TM-01 shared `team{NN}` namespace | High, gap | Removed by dedicated pods and accounts (S-01) |
| TM-02 portal holds a Proxmox password | High, partial | Held by practice, with narrow rights and no reset or power (S-02) |
| TM-03 Battleship machine route | High, partial | Narrowed to deploy and teardown (S-03) |
| TM-04 approval is one tag | High, partial | Three keys (S-04) |
| TM-05 network and capacity | High, gap | Network is a requirement, to test (S-05); capacity has a budget |
| TM-06 window end | High, partial | Fixed order, blocks the next session (S-06) |
| TM-07 host identity and join tokens | Medium, gap | Emailed links (S-09) |
| TM-08 payment | Medium, partial | S-08 |
| TM-09 AI grading, TM-10 AI red team | Medium and High | Moved to `practice-ai-addon` |
| TM-11 reset and power as a lever | Medium, partial | Teams do not submit Battleship jobs (S-14) |
| TM-12 audit | Low, gap | S-15 |
| Public webhook beside the portal's Authentik admin token | Medium | Removed by the separate app (S-08, S-22) |
| Play worker inside the portal | Medium | Replaced by practice's own worker (S-22) |

## 8. Not verified

Read from the repositories and from Proxmox's, Quotient's and Authentik's sources and documentation. Nothing was run against a cluster.

- The items marked "to test" under "Assumptions" in `design.md`.
- What the org's proxy allows and blocks today.
- Whether practice, the portal and Battleship can reach each other without the public address.
- Production pool naming. Battleship's default is `pool-{team}`; the portal's `team_pool_id` says `blue-team-{NN}`.
- Production address plan. Battleship's default external subnet is `10.50.1{team}`; the cheat sheet and Quotient's examples use `10.100.1X`.
- The reach of `Datastore.Allocate`, from memory of Proxmox's privilege list.
- That a password change ends an account's open Authentik sign-ins, read from Authentik's source and not run.
