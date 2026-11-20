# Design

## Context

See `proposal.md` for motivation. This builds on `self-service-team-play`, which gives: the practice app, a Go program that serves the web pages and runs a worker loop; environments published as one bundle; a practice Quotient rebuilt per session on its own VM with an agent that takes rebuild and wipe from the practice app; and pod networks that only scoring and the jump box can reach into.

Quotient holds inject submissions. Its admin API lists injects and downloads submissions (`www/router.go`). It does not grade.

## Goals / Non-Goals

**Goals:**
- Grading and red team activity that need no staff during a session.
- Nothing a customer uploads, and nothing a rooted VM answers, can steer either beyond producing a score or choosing among actions staff wrote.

**Non-Goals:**
- A red team that invents its own attacks.
- Grades that count for anything outside the session.
- Human review of grades.

## Decisions

### 1. Grading runs in the practice worker

- The worker reads new submissions from the session's Quotient with that session's admin sign-in.
- Text is taken out of the file with size, page and time limits. The file is treated as hostile.
- The model is called with no tools. The submission is passed as data, apart from the instructions. The reply must match a fixed shape: a score and a comment per rubric item.
- Code, not the model, clamps each item to its bounds and adds them up.
- Scores and feedback are stored in the practice app against the booking and pod, and shown on the pod page as escaped plain text.
- Limits: a fixed number of graded submissions per inject per pod, and a token budget per booking. Past either, submissions are kept and marked not graded.

Alternative: grade inside Quotient. Rejected: it would put an AI key and outbound calls on the VM that faces customers' boxes.

### 2. The red team runner is a dumb executor on the scoring VM

- The agent on the scoring VM starts a runner container when it rebuilds for an add-on session, and removes it at wipe. A new one each session.
- The runner carries out named actions from the environment's list against an address, and reports what happened. That is all it can do.
- It holds no AI provider key and has no route to the internet. It reaches the booking's pod networks, as scoring does, and nothing else.
- The practice worker decides which action runs next and against which pod. It computes the address from the booking's pod numbers each time, and checks the booking is still running first.
- If a model helps choose the order, it picks an action by name from the list. A target's reply is recorded as text. It is never run, and never used as an address.
- A VM that was reset is left alone until it is running again. End cancels whatever is in flight.

Why on the scoring VM: it is the one machine that already reaches into the session's pods and nowhere else. Why not in the worker: the worker holds the practice app's secrets and must not open connections to rooted VMs.

Alternative: let a model drive a shell in the runner. Rejected: a rooted target could then feed it instructions.

### 3. Action lists and rubrics are part of the environment

Staff write them and publish them with the environment. An action list is code that runs from inside the org against customers' VMs, so it is reviewed like code. An environment without both is published without the add-on, and the add-on is not offered for it.

### 4. Findings and feedback stay with the pod

Both are stored against booking and pod and shown only on that pod's link. The shared scoreboard shows neither. Nothing is written to Quotient's scores, so the add-on never changes a pod's standing.

## Risks / Trade-offs

- **Prompt injection in a submission** → no tools, fixed reply shape, scores clamped in code. The worst outcome is a wrong practice score.
- **Feedback carrying markup** → shown as escaped text.
- **A rooted VM answering the runner with instructions** → replies are data; the runner only knows named actions.
- **The runner as a way out of a pod** → no internet, no key, one session's life, reach limited to the booking's pods.
- **Submissions leaving the org** → host and sub-teams are told before they play; the terms name the provider.
- **Cost running past $10** → the cap and the budget.
- **An action list that damages more than intended** → written by staff, reviewed, and run only against pods that are torn down afterwards.

## Open Questions

- Which AI provider and model.
- How long the provider keeps submissions, to state in the terms.
- The cap and budget figures.
