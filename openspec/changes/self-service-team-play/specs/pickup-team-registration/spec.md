# Spec Delta

## Purpose

Defines who plays in a session: the host's pod and up to 8 sub-team pods, how sub-teams are invited by email, what each pod's link shows, and the life of the session accounts the pods sign in with.

## ADDED Requirements

### Requirement: Host pod and up to eight sub-team pods
The system SHALL give every booking one pod for the host's team and SHALL let the host add up to 8 sub-teams, each with its own pod. Pods SHALL come from the session pod range, which official events never use.

#### Scenario: Host adds sub-teams
- **WHEN** the host adds 8 sub-teams to a booking
- **THEN** the booking has 9 pods, each with a different number from the session pod range

#### Scenario: Ninth sub-team
- **WHEN** the host tries to add a ninth sub-team
- **THEN** the system refuses and says the limit is 8

### Requirement: Sub-teams are invited by email
The system SHALL let the host name an email address for each sub-team and SHALL send that address a secret link for that sub-team's pod. The host SHALL be able to change or remove a sub-team until the session starts; the pod's earlier link then stops working.

#### Scenario: Host invites a sub-team
- **WHEN** the host enters an email address for a sub-team
- **THEN** that address receives a link for that sub-team's pod

#### Scenario: Host corrects a mistyped address
- **WHEN** the host replaces a sub-team's email address before the session starts
- **THEN** the old link stops working and the new address receives a new one

#### Scenario: Change after Start
- **WHEN** the host tries to change or remove a sub-team after the session has started
- **THEN** the system refuses

### Requirement: Invitation emails are limited
The system SHALL send no more than a fixed number of invitation emails for one booking, counting every change of a sub-team's address.

#### Scenario: Host keeps changing addresses
- **WHEN** a host changes sub-team addresses until the booking's limit of invitation emails is reached
- **THEN** further changes are refused and no more invitations are sent

### Requirement: A pod link shows only its own pod
The system SHALL show, through a pod's link, only that pod's sign-in details, packet and results, and the scoreboard its booking shares. Every pod SHALL have its own link, the host's pod included. The booking link SHALL manage the booking and show no pod's sign-in details.

#### Scenario: Link opened before Start
- **WHEN** a sub-team opens its link before the session starts
- **THEN** it sees the date, window and environment name, and no sign-in details or packet

#### Scenario: Link opened during the session
- **WHEN** a sub-team opens its link while the session runs
- **THEN** it sees its own pod's sign-in details and the packet, and nothing else of any other pod

#### Scenario: Host shares access with teammates
- **WHEN** the host passes the host pod's link to teammates
- **THEN** they see the host pod's sign-in details and cannot manage the booking

#### Scenario: Booking link during the session
- **WHEN** the host opens the booking link while the session runs
- **THEN** it shows the booking's state and controls and no pod's sign-in details

### Requirement: Each pod accepts the terms before its sign-in details are shown
The system SHALL show a pod's sign-in details and VM logins only after someone using that pod's link has accepted the terms of use, and SHALL record the acceptance with the pod.

#### Scenario: Terms not yet accepted
- **WHEN** a pod's link is opened during the session and the terms have not been accepted for that pod
- **THEN** the page shows the terms and no sign-in details

### Requirement: Session accounts live only for the session
The system SHALL switch on a booking's session accounts with new passwords when the session starts, and at its end SHALL switch them off, end their open sign-ins everywhere they sign in, and replace their passwords. Session accounts SHALL be separate from official team accounts.

#### Scenario: Session starts
- **WHEN** a session starts
- **THEN** only that booking's session accounts are switched on, each with a password not used before

#### Scenario: Session ends
- **WHEN** a session ends
- **THEN** its accounts are switched off, their open sign-ins stop working, and their passwords are replaced

#### Scenario: Sign-in from an earlier session
- **WHEN** someone who played in an earlier session tries to use that session's sign-in or password during a later session on the same pod number
- **THEN** access is refused

### Requirement: Session accounts hold nothing in official systems
A session account SHALL NOT be a member of any official team. It SHALL be able to sign in to the team console, the jump box and its session's scoring, and to nothing else the org runs: not the portal's team pages, the official scoring engine, Battleship, or any official team's pod.

#### Scenario: Session account signs in to the portal
- **WHEN** someone signs in to the portal with a session account
- **THEN** no team page, ticket form or incident report form is available to them

#### Scenario: Session account tries Battleship
- **WHEN** someone tries to sign in to Battleship with a session account
- **THEN** the sign-in is refused before any page is shown
