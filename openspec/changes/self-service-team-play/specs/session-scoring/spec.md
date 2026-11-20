# Spec Delta

## Purpose

Gives each self-service session its own scoring: service checks and mock injects run from the environment's configuration on a scoring instance that starts empty, is used by that session only, and is wiped afterwards.

## ADDED Requirements

### Requirement: Each session scores on an instance that starts empty
The system SHALL give each session a scoring instance holding no teams, scores, submissions or start time from any earlier session, and SHALL NOT use the official scoring engine for a session.

#### Scenario: Second session on the same day
- **WHEN** a session starts after an earlier session has ended
- **THEN** its scoring shows no score, submission or inject timing from the earlier one

#### Scenario: Official engine untouched
- **WHEN** a session starts, runs and ends
- **THEN** the official scoring engine's configuration, teams, scores and injects are unchanged

### Requirement: Checks and injects come from the environment
The system SHALL configure a session's scoring from the chosen environment: its service checks for every pod of the booking, and its inject schedule for the window length.

#### Scenario: Scoring for a 3-pod booking
- **WHEN** a session with 3 pods starts
- **THEN** scoring checks the environment's services on those 3 pods and no others

### Requirement: Injects open on the environment's schedule from the session's start
The system SHALL open and close each inject at the environment's offsets counted from the moment scoring starts for the session.

#### Scenario: Inject due 30 minutes in
- **WHEN** an environment schedules an inject to open 30 minutes after start and scoring starts at 10:05
- **THEN** the inject opens at 10:35

### Requirement: Each pod signs in to scoring with its session account
The system SHALL let each pod sign in to its session's scoring with the same session account it uses for the console, with no second password. A pod SHALL see and submit only for its own team.

#### Scenario: Pod submits an inject
- **WHEN** a pod signs in to scoring with its session account and submits an inject response
- **THEN** the submission is recorded for that pod's team

#### Scenario: Official team account tries the practice scoring
- **WHEN** someone signs in to a session's scoring with an official team account
- **THEN** they are placed on no team and can submit nothing

#### Scenario: Pod asks for another team's detail
- **WHEN** a pod requests another team's service detail or submissions
- **THEN** the request is refused

### Requirement: Scoring reaches only the session's pods
A session's scoring SHALL send checks only to the addresses of that booking's pods.

#### Scenario: Check target
- **WHEN** scoring runs a round of checks
- **THEN** every check goes to an address inside one of the booking's pods

### Requirement: Results outlive the instance
The system SHALL, before wiping a session's scoring instance, keep each pod's final service scores and inject submissions, and SHALL make a pod's own results available through that pod's link for the retention period.

#### Scenario: Session ends
- **WHEN** a session ends
- **THEN** each pod is emailed that its results are ready, and its link shows its scores and its own submissions

#### Scenario: Retention period passes
- **WHEN** the retention period after a session has passed
- **THEN** the pod links stop working and the kept results are deleted

### Requirement: Scored checks begin only once the pods answer
The system SHALL run unscored trial checks after the pods are deployed and SHALL start scored checks and the inject clock only when every pod of the booking has passed them. If a pod has not passed within the start limit, the start SHALL be treated as failed.

#### Scenario: Boxes still booting
- **WHEN** a pod's services do not yet answer two minutes after deployment
- **THEN** no scored check has run, no penalty is recorded, and no inject has opened

#### Scenario: A pod never comes up
- **WHEN** a pod's services still fail the trial checks at the start limit
- **THEN** the booking is handled as a failed start

### Requirement: Pods in a booking share a scoreboard
The system SHALL show every pod of a booking each pod's total score and which of its services are up, during the session and with the results afterwards. It SHALL NOT show one pod another's service detail or submissions.

#### Scenario: Pod looks at the scoreboard
- **WHEN** a pod opens the scoreboard during the session
- **THEN** it sees every pod's total and service status, named by pod

#### Scenario: Results after the session
- **WHEN** a pod opens its link after the session
- **THEN** it sees the booking's final standings and only its own submissions
