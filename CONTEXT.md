# Local Content Share

Local Content Share keeps small pieces of content and files consistent across browser and Android clients through one NAS-backed collection.

## Language

**Content Item**:
A stable object in the shared collection, identified by an immutable UUID and changed through revisions.
_Avoid_: Card, row, path identity

**Content Mutation**:
One accepted creation, edit, rename, favorite change, or deletion of a Content Item that advances its revision and the shared event sequence.
_Avoid_: Refresh, update message

**Content Event**:
The ordered notification describing an accepted Content Mutation to connected clients.
_Avoid_: Refresh signal, reload event

**Transfer Task**:
The durable lifecycle that moves one file into the shared Files collection and either commits it completely or leaves no visible Content Item.
_Avoid_: Temporary upload, download row

**Device Session**:
One active browser page associated with a persistent Browser Device.
_Avoid_: Device, tab identity

**Browser Device**:
The persistent identity shared by pages in one browser profile, including its display name and lock state.
_Avoid_: Fingerprint, Device Session
