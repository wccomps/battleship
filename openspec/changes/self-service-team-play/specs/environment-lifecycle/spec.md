# Spec Delta

## Purpose

Defines an environment, the one bundle staff publish and teams play, and what happens to a session's pods from Start to teardown: deployment, release of sign-ins, logins and the packet, name resolution, the network limits on session pods, and the ordered end of a session.

## ADDED Requirements

### Requirement: An environment bundles everything a session needs
An environment SHALL hold, as one unit: the Battleship environment whose VMs are copied into each pod, the team packet, the logins teams use on those VMs, the scoring configuration, the name resolution settings, and an inject schedule for each window length it supports.

#### Scenario: Staff publish an environment
- **WHEN** staff publish an environment
- **THEN** every part of it is checked and published together, and none can be booked on its own

### Requirement: Only published environments can be played
The system SHALL offer an environment to hosts only when staff have published it, every VM in its set carries the approval mark, and it is complete for the window length. The approval mark alone SHALL NOT make an environment playable.

#### Scenario: Published and complete
- **WHEN** a host views environments for an 8-hour booking
- **THEN** the list holds only published environments with approved VMs and an 8-hour inject schedule

#### Scenario: Approved VMs, not published
- **WHEN** a set carries the approval mark but staff have published no environment for it
- **THEN** it is not offered and cannot be deployed for a session

#### Scenario: No schedule for the window length
- **WHEN** a published environment has no 4-hour inject schedule
- **THEN** it is not offered for a 4-hour booking

### Requirement: Sessions get the environment as published
A session SHALL deploy the environment's templates as they were when it was published. Changes staff make to the environment's VMs afterwards SHALL NOT reach any session until the templates are rebuilt and the environment is published again. An environment whose templates are missing or have been replaced since it was published SHALL NOT be offered, and staff SHALL be alerted.

#### Scenario: Staff edit a VM after publishing
- **WHEN** staff change a VM in a published environment without rebuilding its template and publishing again
- **THEN** sessions keep receiving the version that was published

#### Scenario: Template rebuilt without publishing again
- **WHEN** a published environment's template is rebuilt and the environment is not published again
- **THEN** the environment is no longer offered, a host who had chosen it is asked to choose another, and staff are alerted

#### Scenario: Environment torn down
- **WHEN** the Battleship environment behind a published environment is torn down
- **THEN** it is no longer offered and staff are alerted

### Requirement: A deploy carries the template digests it was listed with
Every deploy the system asks Battleship for SHALL carry, for each template of the set, the config digest Battleship listed for it. Battleship SHALL refuse a deploy without them, and SHALL refuse a deploy whose templates' digests differ from them, both when the deploy is submitted and again when Battleship's worker takes up the job.

#### Scenario: Template changed since it was listed
- **WHEN** a template of the environment is rebuilt after the system listed the sets, before the deploy is submitted or while it waits to run
- **THEN** Battleship refuses the deploy and clones nothing from the changed template

#### Scenario: Deploy without digests
- **WHEN** the system asks Battleship for a deploy that carries no digests
- **THEN** Battleship refuses it

### Requirement: Start deploys the environment to the booking's pods
The system SHALL, when the host presses Start inside the booked window, deploy the chosen environment to every pod of the booking, take each VM's baseline, and start the VMs. Start SHALL be refused outside the window.

#### Scenario: Start inside the window
- **WHEN** the host presses Start during the window for a booking with 4 pods
- **THEN** the environment's VMs are created in those 4 pods, each with a baseline, and started

#### Scenario: Start outside the window
- **WHEN** Start is pressed before the window opens or after it has closed
- **THEN** the system refuses and creates nothing

#### Scenario: Start pressed twice
- **WHEN** Start is pressed again while a start is under way or the session is running
- **THEN** nothing further happens

### Requirement: Publication is checked again at Start
The system SHALL refuse to start a session whose environment is no longer published, whose VMs no longer all carry the approval mark, or whose templates have changed since publishing, and SHALL treat that as a failed start.

#### Scenario: Approval withdrawn after booking
- **WHEN** an environment's approval is withdrawn after a booking chose it and the host presses Start
- **THEN** nothing is deployed and the booking is handled as a failed start

### Requirement: Sign-ins, VM logins and packet are released when the pods are ready
The system SHALL show each pod, on its link, its session sign-in, the environment's VM logins and the packet only after the pods are deployed and scoring is ready, and SHALL email each pod that the session has begun. Neither the sign-in nor the VM logins SHALL be sent by email.

#### Scenario: Setup completes
- **WHEN** the pods are deployed and scoring is ready
- **THEN** each pod's link shows its session sign-in, the VM logins and the packet, and each pod is emailed a notice without them

#### Scenario: Before setup completes
- **WHEN** a pod's link is opened while the pods are still deploying
- **THEN** it shows progress and no sign-in, VM logins or packet

### Requirement: VM logins come from the environment, never from the VMs
The VM logins shown to teams SHALL be the ones published with the environment. Logins written in a master VM's notes SHALL NOT be shown to teams.

#### Scenario: Master notes hold a staff login
- **WHEN** a master VM's notes hold a login staff use while building it
- **THEN** that login appears neither on the pod page nor in the pod's VMs' notes

### Requirement: Names resolve to the right pod
The system SHALL resolve the environment's host names, for scoring and for teams on the jump box, to the addresses of the pod they are asked about, using the environment's name resolution settings.

#### Scenario: Scoring checks a named service
- **WHEN** scoring checks a service by its host name for pod 52
- **THEN** the name resolves to pod 52's address

#### Scenario: Team looks up a box from the jump box
- **WHEN** pod 51's members look up one of the environment's host names from the jump box
- **THEN** it resolves to pod 51's box

### Requirement: Session pods are cut off from everything else
Session pod networks SHALL have no route to any other pod, including the other pods of the same booking, to official pods, to the cluster's management network, or to the systems that run the org's own applications. Only scoring and its name resolution, and the pod's own members through the jump box, SHALL be able to open connections into a pod.

#### Scenario: VM in a session pod tries to reach inside the org
- **WHEN** a VM in a session pod opens a connection to an official pod or the cluster's management addresses
- **THEN** the connection does not succeed

#### Scenario: VM tries to reach another pod of the same booking
- **WHEN** a VM in one pod opens a connection to a VM in another pod of the same booking
- **THEN** the connection does not succeed

### Requirement: Session pods reach the internet only through the org's proxy
A VM in a session pod SHALL have no way to the internet except the proxy the org controls. Every other outbound connection SHALL be blocked. The proxy SHALL record each request with the pod it came from.

#### Scenario: Direct connection to the internet
- **WHEN** a VM in a session pod opens a connection straight to an internet address
- **THEN** the connection does not succeed

#### Scenario: Request through the proxy
- **WHEN** a VM in a session pod makes a web request through the org's proxy
- **THEN** the request is handled by the proxy's rules and recorded against that pod

### Requirement: A session ends in a fixed order
The system SHALL end a session at the end of its window, or earlier when the host ends it, in this order: stop scoring, switch off the session accounts and end their sign-ins, tear down the booking's pods, wipe the scoring instance, send results. Only the booking's own pods SHALL be torn down.

#### Scenario: Window ends
- **WHEN** a running session reaches the end of its window
- **THEN** the steps run in order and no VM of the booking remains

#### Scenario: Host ends early after someone has signed in
- **WHEN** the host ends the session before the window closes and a session account has signed in
- **THEN** the same steps run and no refund is made for the unused time

### Requirement: A session that cannot be cleaned up blocks the next one
The system SHALL keep bookings closed and alert staff while any step of ending a session has not completed, and SHALL NOT start another session until the earlier session's pods are confirmed gone and its accounts are switched off.

#### Scenario: Teardown fails
- **WHEN** a session's pods cannot be torn down
- **THEN** staff are alerted, the attempt is repeated, and no other session can start until it succeeds

### Requirement: Start is all or nothing
The system SHALL treat a start as failed, tear down everything it created and refund the booking when any pod of the booking cannot be deployed or does not come up. It SHALL NOT run a session on some of a booking's pods.

#### Scenario: One pod of nine fails to deploy
- **WHEN** eight of a booking's nine pods deploy and one does not
- **THEN** all nine are torn down and the booking is handled as a failed start

### Requirement: A booking's pods fit the session capacity
An environment SHALL record the memory, processors and disk one pod of it needs. The system SHALL limit a booking to the number of pods whose total fits the session capacity staff have set, and SHALL say so when the host adds sub-teams.

#### Scenario: Large environment
- **WHEN** a host has chosen an environment of which only 5 pods fit the session capacity and tries to add a fifth sub-team
- **THEN** the system refuses and says this environment allows 5 pods

#### Scenario: Host switches to a larger environment
- **WHEN** a host with 9 pods chooses an environment of which only 5 fit
- **THEN** the system refuses the change until the booking has 5 pods or fewer

### Requirement: Nothing else the org runs reaches into a session pod
The official scoring engine, the proxy and name resolution official teams use, and official team accounts SHALL have no route into a session pod.

#### Scenario: Official team path tried against a session pod
- **WHEN** a connection to a session pod is attempted through the proxy or name resolution that official teams use
- **THEN** the connection does not succeed

### Requirement: Pods are warned before the window ends
The system SHALL tell every pod of a running session 30 minutes and 5 minutes before its window ends, on the pod's link and in scoring.

#### Scenario: Thirty minutes left
- **WHEN** a running session has 30 minutes left
- **THEN** every pod's link and the scoring pages show that the session ends in 30 minutes

### Requirement: The host is told when a chosen environment is withdrawn
The system SHALL email the host at once when the environment a confirmed booking has chosen stops being offered, and SHALL let the host choose another or cancel for a refund.

#### Scenario: Environment withdrawn a week before the window
- **WHEN** a confirmed booking's environment is withdrawn before its window
- **THEN** the host is emailed and the booking page asks for another environment
