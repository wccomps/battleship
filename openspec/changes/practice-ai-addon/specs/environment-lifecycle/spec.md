# Spec Delta

## MODIFIED Requirements

### Requirement: Session pods are cut off from everything else
Session pod networks SHALL have no route to any other pod, including the other pods of the same booking, to official pods, to the cluster's management network, or to the systems that run the org's own applications. Only scoring and its name resolution, the pod's own members through the jump box, and, in add-on sessions, the red team SHALL be able to open connections into a pod.

#### Scenario: VM in a session pod tries to reach inside the org
- **WHEN** a VM in a session pod opens a connection to an official pod or the cluster's management addresses
- **THEN** the connection does not succeed

#### Scenario: VM tries to reach another pod of the same booking
- **WHEN** a VM in one pod opens a connection to a VM in another pod of the same booking
- **THEN** the connection does not succeed

#### Scenario: Red team in a session without the add-on
- **WHEN** a session was booked without the add-on
- **THEN** no red team component exists and nothing but scoring and the pod's own members can connect into its pods
