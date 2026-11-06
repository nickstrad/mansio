"""Author DP-02 only: the Fable revision of the planes dataset.

Part A below is the DP-01 generator (agent-work/items/dp-01-planes/build-dataset.py) verbatim except for the
dataset identity, output paths and source pins; run on its own it reproduces mansio-planes-proposal byte for byte
after deno fmt, which is how "carried over unchanged" is kept honest. Part B applies modifications M1–M8 from
docs/architecture/scale-out-comparison-fable.md. Part C orders, links, trailers and writes the bundle and the
inventory. Run from the repository root; format outputs with deno fmt --config deno.json.
"""
import copy
import hashlib
import json
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
BUNDLE = ROOT / 'pages/public/architectures/mansio-planes-fable.json'
CONTENT = ROOT / 'pages/content/mansio-planes-fable'
# Every carried-over source is re-pinned to main at 3d25004 (Fable's comparison commit); every path exists there.
BASE = '3d25004d43406519571ce51ad534ec0e92a54d7e'
BRIEF = BASE
# The DP-02 brief is pinned to the commit that recorded it (b2b65b0), one commit before this dataset.
DP02_BRIEF = 'b2b65b04cedd4899c4ad895305f3e745026fa010'
SHIFT = ('A tenant owns zero or more databases (0..N). The database is the unit of placement, isolation, '
         'backup and lifecycle. One-file-per-tenant is superseded in this proposal; the lanes dataset stays unchanged.')
STE_SHIFT = ('A tenant can have zero or more databases. Place, isolate, back up and manage each database separately. '
             'This proposal replaces one file for each tenant with one file for each database. The lanes dataset does not change.')
SOURCES = []


def source(id, path, title, rev=BASE, notes=None):
    s = dict(id=id, title=title, path=path, revision=rev,
             url=f'https://github.com/nickstrad/mansio/blob/{rev}/{path}')
    if notes:
        s['notes'] = notes
    SOURCES.append(s)


source('src-brief', 'agent-work/items/dp-01-planes/brief.md', 'DP-01 authorized design brief', BRIEF)
source('src-addendum', 'agent-work/items/dp-01-planes/addendum-1.md',
       'DP-01 addendum 1: VM agents and control-plane routing / placement')
source('src-review', 'agent-work/items/dp-01-planes/review-astra.md', 'DP-01 independent review and repair requirements')
source('src-lanes', 'pages/public/architectures/mansio-lanes-proposal.json', 'Preserved scale-out dataset')
source('src-lanes-inventory', 'pages/content/mansio-lanes-proposal/inventory.md', 'Scale-out decisions P1–P17')
source('src-router', 'platform/tenants/routing.ts', 'Landed TenantStorageRouter seam; future ControlPlaneRouter')
source('src-classifier', 'platform/sqlite_ffi/classify.ts', 'Landed operation-IR classifier')
source('src-ffi', 'platform/sqlite_ffi/effect.ts', 'Landed scoped FFI and cancellation wrappers')
source('src-storage', 'platform/sqlite.ts', 'Landed Storage, connection gate and deadlines')
source('src-decisions', 'docs/knowledge/decisions.md', 'Router seam and Postgres handwritten SQL decisions')

b = dict(schemaVersion=1, id='mansio-planes-fable',
         title='Fable revision: control/data planes with route grants (M1–M8 applied)',
         revision='dp-02-fable-2026-10-20', defaultDiagramId='planes-context',
         model=dict(elements=[], relationships=[]), diagrams=[], documents=[], sources=SOURCES, terms=[])
elements = {}
relationships = {}
provenance = {}


def doc(id, title, plain, ste, sources):
    b['documents'].append(dict(id=id, title=title, plain=plain + '\n\n' + SHIFT,
                               ste=dict(text=ste + '\n\n' + STE_SHIFT,
                                        issue='ASD-STE100 Issue 9', reviewStatus='draft'),
                               termIds=[], sourceIds=sources))
    return id


def element(id, kind, name, description, ste, parent=None, status='planned',
            sources=None, tech=None, tags=None, notes=None, origin=None, **extra):
    sources = sources or ['src-brief']
    e = dict(id=id, kind=kind, name=name, description=description, status=status,
             tags=tags or [], plannedNotes=notes or [], sourceIds=sources,
             docIds=[doc('doc-' + id, name, description, ste, sources)], **extra)
    if parent:
        e['parentId'] = parent
    if tech:
        e['technology'] = tech
    if kind == 'container':
        e['containerType'] = 'datastore' if id in ['metadata', 'files'] else 'application'
    b['model']['elements'].append(e)
    elements[id] = e
    provenance[id] = origin or description
    return id


def rel(id, src, dst, label, tech, sources=None, status='planned', origin=None, **extra):
    r = dict(id=id, sourceId=src, targetId=dst, label=label, technology=tech, status=status,
             tags=[], plannedNotes=[], docIds=[], sourceIds=sources or ['src-brief'], **extra)
    del r['tags']
    b['model']['relationships'].append(r)
    relationships[id] = r
    provenance[id] = origin or label
    return id


external = ['external']
element('platform', 'softwareSystem', 'Database platform: control plane + data plane',
        'Management uses the control plane. Queries use the data plane. Both address databases by tenant and database identity; clients never select a hosting node.',
        'Use the control plane to manage databases. Use the data plane to query a database. The service selects its node.')
element('operator', 'person', 'Customer administrator', 'Creates tenants and databases, inspects state and requests database lifecycle operations.',
        'Create or remove tenants and databases. Inspect their state. Request a move or a backup.')
element('caller', 'person', 'Customer query client', 'One session can address any authorized database of its tenant by name or ID through the same query endpoint.',
        'Use the same endpoint and session to query any database that you are permitted to use.')
element('backups', 'softwareSystem', 'Per-database backup store',
        'Proposed verified snapshots and optional later WAL frames, keyed by immutable database ID and generation. Retention and erasure are explicit per-database policies.',
        'Keep a verified backup for each database. Identify the database and its generation. Apply its retention and deletion policy.',
        tags=external, sources=['src-brief', 'src-lanes'])
element('control', 'container', 'Control plane — management API',
        'Proposed authenticated management service: tenant/database CRUD, configuration, inspection, provisioning and lifecycle reconciliation. It does not run customer queries.',
        'Authenticate management requests. Create, configure and inspect tenants and databases. Reconcile lifecycle operations.',
        parent='platform', tech='Proposed Deno service; HTTPS JSON')
element('data', 'container', 'Data plane — query gateway',
        'Proposed stable access path: authenticate a session, authorize (tenant, database), resolve placement at open and forward to the owner node. Refresh placement after a move.',
        'Authenticate the session. Check permission for the tenant and database. Find the owner node when the database opens. Refresh the location after a move.',
        parent='platform', tech='Proposed Deno service; HTTPS JSON')
element('metadata', 'container', 'Registry and placement metadata',
        'Proposed Postgres metadata: tenants; databases with unique (tenant_id, name), immutable database_id, state and generation; placement (database_id, owner_node, epoch); durable operation records. Hand-written SQL, no ORM. State transitions use compare-and-swap transactions.',
        'Use Postgres and hand-written SQL. Do not use an ORM. Store each tenant, database, owner node, epoch and operation state. Change related records in one transaction.',
        parent='platform', tech='Postgres; hand-written SQL; no ORM', tags=external,
        sources=['src-brief', 'src-lanes', 'src-decisions'])
element('node', 'container', 'Node execution agent + lane supervisor',
        'Proposed service on each hosting node: validates database ID, owner epoch and active state before admission; enforces per-database gates; provisions files; drains handles; spawns and supervises child-process statement lanes. A shared lane is process fault containment, not a database isolation boundary.',
        'Check database identity, owner epoch and active state before execution. Stop new work for a fenced database. Drain its handles. Manage files and child processes.',
        parent='platform', tech='Proposed Deno process; authenticated internal RPC; child-process IPC', tags=external,
        sources=['src-brief', 'src-lanes'])
element('lanes', 'container', 'Statement lane processes',
        'Proposed child processes execute statements on node-local database files through the landed FFI layer. Handles, write queues, transactions and idempotency records are keyed by database ID, not tenant. A lane can serve several databases; killing it affects those requests but does not merge file lock domains.',
        'Run statements in child processes. Keep handles and write queues for each database separately. A process failure can affect several requests. Each database still has a separate file.',
        parent='platform', tech='Proposed Deno child processes; system libsqlite3 via Deno FFI', tags=external,
        sources=['src-brief', 'src-lanes', 'src-ffi'])
element('files', 'container', 'Isolated database SQLite files',
        'Only the hosting VM agent (process) and its private workers access one SQLite database file and its WAL/SHM sidecars per immutable database ID. File paths are server-issued capabilities; user names are not paths. Each file has its own transaction and writer-lock domain. Delete closes handles and removes sidecars too.',
        'Keep one SQLite file for each database. Keep its WAL and SHM files with it. Use a server-issued path. Close handles before removing all these files.',
        parent='platform', tech='Node-local SQLite WAL files', tags=external,
        sources=['src-brief', 'src-router', 'src-lanes'])

# Control-plane internals.
element('management', 'component', 'Management ingress + authorization',
        'Authenticates administrators, checks tenant/database management grants and accepts idempotent operation IDs. Returns durable operation status for asynchronous lifecycle requests.',
        'Authenticate the administrator. Check permission. Accept an operation ID. Return the recorded operation state.',
        parent='control', tech='Proposed HTTPS JSON API')
element('tenant-registry', 'component', 'Tenant registry',
        'Create, configure, inspect and drop tenants. A tenant owns 0..N database records. Tenant deletion is rejected until every owned database has completed deletion; it never silently abandons files.',
        'Create, configure and inspect tenants. A tenant can have no databases or many databases. Reject tenant deletion until all its databases are deleted.',
        parent='control', tech='Proposed registry logic; Postgres hand-written SQL')
element('database-registry', 'component', 'Database registry',
        'Create/drop databases for a tenant; configure and inspect one database. Resolve tenant-scoped names to immutable IDs. Owns desired state and durable operation records; create starts provisioning, while drop starts lifecycle deletion.',
        'Create, configure and inspect a database for a tenant. Resolve its name to its ID. Record the requested state and operation. Start provisioning or deletion.',
        parent='control', tech='Proposed registry logic; Postgres hand-written SQL')
element('provisioner', 'component', 'Database provisioner',
        'Select a node per database; create its file and schema under a non-active generation; verify node readiness; then atomically publish ACTIVE placement. Idempotent reconciliation cleans failed partial files before name reuse.',
        'Select a node. Create and verify the database file. Publish an active placement only after the node is ready. Reconcile partial files if creation fails.',
        parent='control', tech='Proposed reconciler; authenticated internal RPC', sources=['src-brief', 'src-lanes'])
element('lifecycle', 'component', 'Database lifecycle controller',
        'Move, suspend/resume, backup/restore and delete individual databases. Persist operation intent; block opens; fence and drain the source; checkpoint/copy/verify; flip owner and epoch transactionally only after source fencing and target readiness. Require a reachable source to acknowledge fencing in this first design; otherwise leave unavailable. No automatic failover is claimed.',
        'Record each operation. Stop new opens and drain the database. Verify copied data before changing its owner. Require the source to acknowledge the fence. Keep the database unavailable if the source cannot do this.',
        parent='control', tech='Proposed durable state machine; Postgres SQL + internal RPC', sources=['src-brief', 'src-lanes'])

# Query path and node-local execution: components of distinct processes stay typed as such.
element('ingress', 'component', 'Query ingress / result return',
        'Stable query API accepts a tenant-scoped database name or ID and session credentials. Carries a request ID, operation IR, compiled SQL/binds and deadline; returns rows or typed errors through the same access path.',
        'Accept the database name or ID and session credentials. Keep the request ID and deadline. Return rows or an error through the same endpoint.',
        parent='data', tech='Proposed HTTPS JSON API')
element('authorization', 'component', 'Session authentication + database authorization',
        'Authenticate the session and derive trusted tenant identity. Check the requested database belongs to that tenant and is allowed by its grants. Clients cannot supply a trusted tenant, placement epoch or filesystem path. Recheck grants for each database selection.',
        'Authenticate the session. Derive the tenant identity. Check ownership and permission for the selected database. Do not trust client placement or path values.',
        parent='data', tech='Proposed tenant/database authorization')
element('placement-router', 'component', 'Placement router (ControlPlaneRouter extension)',
        'Proposed database → node resolver, keyed by immutable database ID plus epoch. Resolve at open; invalidate cached locations on stale epoch or a move; fail closed on inactive state. The landed TenantStorageRouter seam isolates storage location from Storage, but resolve(tenantId) does not yet accept a database ID or route remotely. The ControlPlaneRouter implementation and database-aware context/API extension are planned.',
        'Resolve the database location when it opens. Refresh a stale location. Reject an inactive database. The router interface exists today. Database selection and remote routing require new code.',
        parent='control', status='mixed', tags=external, tech='Existing seam; proposed database router',
        notes=['The TenantStorageRouter interface is landed; database-aware ControlPlaneRouter, gateway placement and remote execution are planned.'],
        sources=['src-brief', 'src-router', 'src-decisions'])
element('forwarder', 'component', 'Owner-node forwarder',
        'Send a request to the resolved owner with trusted tenant ID, database ID, epoch, operation IR, SQL/binds, deadline and request ID over authenticated RPC. Return results unchanged. On WrongOwner, refresh and reopen; retries of mutations require per-database request-ID records committed with the mutation. Never blindly replay an ambiguous mutation.',
        'Send the trusted identity, database ID, epoch and statement to the owner. Return the result. Refresh stale placement. Use a database request-ID record before retrying a mutation with an unknown result.',
        parent='data', tech='Proposed authenticated internal RPC', sources=['src-brief', 'src-lanes'])
element('node-admission', 'component', 'Database admission gate + lane supervisor',
        'Node-local planned gate validates owner epoch and database state at each admission, including existing sessions. Per-database quiescing blocks new requests, drains or cancels in-flight work, closes all read/write handles and acknowledges a fence. If a lane must be killed, wait for process exit and fail affected requests; do not claim a fence while an old writer can still run.',
        'Check the database epoch and state on each request. Stop new work when fenced. Drain or cancel work and close all handles. If a process is killed, wait for its exit before acknowledging the fence.',
        parent='node', tech='Proposed node-local gates and child-process supervisor', tags=external,
        sources=['src-brief', 'src-lanes', 'src-storage'])
element('classifier', 'component', 'Statement classifier (landed code)',
        'Existing platform/sqlite_ffi/classify.ts classifies operation IR, not SQL text: bounded plans use blocking execution; unbounded reads use nonblocking execution. Reused unchanged inside proposed lanes; moving it into a lane is not landed.',
        'Classify the operation plan. Use the blocking path for bounded work. Use the nonblocking path for an unbounded read. This code exists. Its execution in a lane is proposed.',
        parent='lanes', status='existing', tech='Existing TypeScript operation-IR classifier', tags=external,
        sources=['src-classifier', 'src-lanes'])
element('execution', 'component', 'Scoped FFI statement execution (landed code)',
        'Existing Deno FFI to system libsqlite3: scoped connections, prepared statements, blocking/nonblocking steps and binding. Reused unchanged in proposed lanes; no separate libsqlite3 container. Deadline wrapper interrupts long reads and waits for execution to settle before reusing the connection.',
        'Use the installed FFI binding to execute a statement. Close scoped resources. Wait for interrupted execution to finish before using its connection again. The proposed lane process does not exist today.',
        parent='lanes', status='existing', tech='Existing Deno FFI; system libsqlite3', tags=external,
        sources=['src-ffi', 'src-storage', 'src-lanes'])
element('deadline', 'component', 'Deadline + interrupt policy',
        'Existing deadline/interrupt behavior is reused; proposed lane watchdog adds supervisor escalation if interruption does not settle. A process kill affects every in-flight request on that lane. Per-database drain acknowledgement waits for completion or confirmed process exit.',
        'Interrupt long reads at the deadline. Wait for execution to finish. The proposed watchdog can ask the supervisor to kill a stuck process. Confirm its exit before acknowledging a drain.',
        parent='lanes', status='mixed', tech='Existing sqlite3_interrupt; proposed lane watchdog', tags=external,
        notes=['Lane watchdog escalation and database-scoped drain acknowledgement are proposed.'],
        sources=['src-ffi', 'src-storage', 'src-lanes', 'src-brief'])

# Model relationships; numbers are stable per model tier and reused by dynamic views.
rel('ctx-manage', 'operator', 'platform', 'Management path → control plane: tenant and database CRUD / lifecycle', 'HTTPS JSON')
rel('ctx-query', 'caller', 'platform', 'Query path → data plane: dynamically access any authorized database', 'HTTPS JSON')
rel('ctx-backup', 'platform', 'backups', 'Per-database verified backup / restore; optional WAL frames later', 'Object-store HTTPS', ['src-brief', 'src-lanes'])
for args in [
    ('c-manage','operator','control','Manage tenants and databases','HTTPS JSON'),
    ('c-query','caller','data','Select any authorized database and query it','HTTPS JSON'),
    ('c-catalog','control','metadata','Tenant/database CRUD, operation state and placement publication','Postgres hand-written SQL'),
    ('c-resolve','data','control','Consult control-plane routing and database authorization','Authenticated internal RPC'),
    ('c-lifecycle','control','node','Provision, fence, drain, copy, delete one database','Authenticated internal RPC'),
    ('c-forward','data','node','Execute on owner: tenant ID, database ID, epoch, request ID, plan, SQL, deadline','Authenticated internal RPC'),
    ('c-lane','node','lanes','Admit database-scoped work; supervise child processes','JSON-lines IPC'),
    ('c-files','node','files','Sole tenant-file access boundary: execute through managed FFI workers','Agent-local FFI / SQLite WAL'),
    ('c-node-files','node','files','Provision, verify, snapshot, restore and remove database files','Node-local filesystem / SQLite backup'),
    ('c-node-backup','node','backups','Publish or fetch verified per-database snapshot','Object-store HTTPS'),
]:
    rel(*args, sources=['src-brief', 'src-lanes'])
for args in [
    ('cp-request','operator','management','Authorized management request and operation status','HTTPS JSON'),
    ('cp-tenant','management','tenant-registry','Create / drop tenant; configure / inspect','In-process call'),
    ('cp-database','management','database-registry','Create / drop tenant database; configure / inspect','In-process call'),
    ('cp-tenants-sql','tenant-registry','metadata','Tenant records; refuse deletion while databases remain','Postgres hand-written SQL'),
    ('cp-databases-sql','database-registry','metadata','Database names / IDs, desired state and idempotent operation records','Postgres hand-written SQL'),
    ('cp-provision','database-registry','provisioner','Reconcile a new database','Durable operation dispatch'),
    ('cp-lifecycle','database-registry','lifecycle','Move, suspend/resume, backup/restore or drop one database','Durable operation dispatch'),
    ('cp-placement','provisioner','metadata','Reserve identity and node; publish ACTIVE placement after readiness','Postgres SQL transaction / compare-and-swap'),
    ('cp-create-file','provisioner','node','Create and verify one database file on selected node','Authenticated internal RPC'),
    ('cp-operation','lifecycle','metadata','Persist state / tombstone; atomically change owner and epoch','Postgres SQL transaction / compare-and-swap'),
    ('cp-node','lifecycle','node','Fence, quiesce, copy/verify, activate or delete addressed database','Authenticated internal RPC'),
]:
    rel(*args)
for args in [
    ('dp-request','caller','ingress','Query a named database using the same session and endpoint','HTTPS JSON'),
    ('dp-auth','ingress','authorization','Authenticate session and authorize tenant / database selection','In-process call'),
    ('dp-auth-sql','authorization','database-registry','Consult control plane: resolve name and verify tenant ownership / grants','Authenticated internal RPC'),
    ('dp-route','authorization','placement-router','Resolve authorized database at open; refresh after relocation','Authenticated control-plane routing RPC'),
    ('dp-placement','placement-router','metadata','Read ACTIVE database → node and epoch; reject inactive state','Postgres hand-written SQL'),
    ('dp-forward','authorization','forwarder','Invoke gateway forwarder with authorized route','In-process call'),
    ('dp-owner','forwarder','node-admission','Owner-node request or WrongOwner / inactive response','Authenticated internal RPC'),
    ('dp-classify','node-admission','classifier','Admit and classify operation IR in a selected lane','JSON-lines IPC; proposed lane placement'),
    ('dp-execute','classifier','execution','Choose bounded blocking or unbounded nonblocking step','Existing classifier path choice; planned lane dispatch'),
    ('dp-deadline','execution','deadline','Apply deadline policy and settle interrupted step','Existing binding cancellation; proposed watchdog integration'),
    ('dp-interrupt','deadline','execution','Interrupt at deadline; escalate if step does not settle','Existing sqlite3_interrupt; proposed escalation'),
    ('dp-escalate','deadline','node-admission','Ask supervisor to kill stuck lane; wait for confirmed exit','Proposed watchdog IPC'),
    ('dp-files','execution','files','Read / write only the selected database SQLite file','Existing Deno FFI / SQLite WAL'),
]:
    rel(*args, sources=['src-brief', 'src-lanes'])
# Only call relationships wholly reused as-is are existing. The process deployment stays planned.
for rid, srcs, status, notes in [
    ('dp-files',['src-ffi','src-router'],'existing',[]),
    ('dp-deadline',['src-ffi','src-storage','src-brief'],'mixed',['Existing deadline policy; proposed integration with a lane watchdog.']),
    ('dp-interrupt',['src-ffi','src-lanes'],'mixed',['Existing interrupt call; proposed escalation on a stuck lane.']),
]:
    relationships[rid].update(sourceIds=srcs,status=status,plannedNotes=notes)

legend = dict(title='Key', entries=[
    dict(id='scope',label='System in scope',description='The database platform is highlighted in the context view.',swatch=dict(shape='box',tone='accent',stroke='solid')),
    dict(id='person',label='Person / customer role',description='A customer administrator or query client.',swatch=dict(shape='person',tone='default',stroke='solid')),
    dict(id='box',label='System, process or component',description='Typed C4 element. A library is inside its process, never a separate container.',swatch=dict(shape='box',tone='default',stroke='solid')),
    dict(id='external',label='External or supporting',description='Grey includes supporting elements outside a component scope; it does not imply third-party ownership.',swatch=dict(shape='box',tone='external',stroke='solid')),
    dict(id='boundary',label='Process / fleet boundary',description='A proposed boundary groups related services or node-local execution.',swatch=dict(shape='boundary',tone='muted',stroke='dashed')),
    dict(id='planned',label='Planned or mixed',description='Dashed means planned or existing with planned changes. The card and notes distinguish them.',swatch=dict(shape='box',tone='default',stroke='dashed')),
    dict(id='existing-edge',label='Existing call',description='Solid relationship: reused landed code, not proof a proposed process exists.',swatch=dict(shape='line',tone='default',stroke='solid',arrow='forward')),
    dict(id='planned-edge',label='Proposed relationship',description='Dashed relationship: unbuilt behavior or changes. Edge numbers are model references, not measurements.',swatch=dict(shape='line',tone='default',stroke='dashed',arrow='forward')),
])
edge_numbers = {}
for prefix in ['ctx-', 'c-', 'cp-', 'dp-']:
    edge_numbers.update({r: n for n, r in enumerate((r for r in relationships if r.startswith(prefix)), 1)})


def diagram(id, kind, title, members, edges, plain, ste, scope=None, parent=None, **extra):
    d = dict(id=id, kind=kind, title=title, status='planned',
             nodes=[dict(elementId=e) for e in members],
             edges=[dict(relationshipId=r,displayNumber=edge_numbers[r]) for r in edges],
             docIds=[doc('doc-' + id,title,plain,ste,['src-brief','src-lanes'])],
             sourceIds=['src-brief','src-lanes'], layout=dict(mode='auto',direction='TB'),
             legend=copy.deepcopy(legend), groups=[], related=[], **extra)
    if scope:
        d['scopeId'] = scope
    if parent:
        d['parent'] = dict(diagramId=parent)
    b['diagrams'].append(d)
    return d


context = diagram('planes-context','context','System context: control plane / data plane',
                  ['operator','caller','platform','backups'],[r for r in relationships if r.startswith('ctx-')],
                  'Proposed database service. Two distinct paths are the headline: administrators manage tenants and databases through the control plane; customers query any authorized database through the data plane. Hosting nodes are hidden from clients; the container view separates these processes. This is a design, not deployed behavior.',
                  'Use the management path for lifecycle operations. Use the query path for database access. Clients do not select nodes. Open the container view to see the two planes and hosting nodes.',scope='platform')
containers = diagram('planes-containers','container','Containers: management plane, query plane and hosting nodes',
                     ['operator','caller','control','data','metadata','node','lanes','files','backups'],
                     [r for r in relationships if r.startswith('c-')],
                     'Control plane owns management state in Postgres with hand-written SQL (no ORM). Data plane consults control-plane routing, forwards to its VM agent and returns results. A fleet node manages database files and child-process lanes. The node boundary represents one of many nodes, not one tenant. Grey supporting containers are internal to the platform, except the backup service; component-view tags make them grey in this view too.',
                     'The control plane changes metadata. The data plane reads metadata and sends queries to owner nodes. A node runs lanes and holds database files. Grey can mean a supporting internal container.',scope='platform',parent='planes-context')
containers['groups']=[dict(id='g-node',label='Hosting node (one of N; database-scoped placement)',memberIds=['node','lanes','files'],status='planned')]
cp = diagram('planes-control-components','component','Control plane: tenant/database CRUD and lifecycle',
             ['operator','management','tenant-registry','database-registry','provisioner','lifecycle','placement-router','metadata','node'],
             [r for r in relationships if r.startswith('cp-')] + ['dp-placement'],
             'Tenant registry owns tenant CRUD; database registry owns tenant-scoped database CRUD, configure and inspect. Postgres metadata is accessed with hand-written SQL, no ORM. Provisioning publishes only verified files. Lifecycle operations move, suspend/resume, back up, restore or delete one database. Tenant drop is restricted until all its databases are deleted. Operations are durable and idempotent across crashes; the control plane reconciles intermediate states.',
             'Keep tenants and their databases in separate records. Use hand-written SQL in Postgres. Publish a new database only after verification. Record lifecycle work so it can resume after a crash. Reject tenant deletion while databases remain.',scope='control',parent='planes-containers')
dp = diagram('planes-data-components','component','Data plane: consult control-plane routing → VM agent',
             ['caller','ingress','authorization','placement-router','forwarder','database-registry','metadata','node-admission','classifier','execution','deadline','files'],
             [r for r in relationships if r.startswith('dp-')],
             'A session selects any authorized database by name/ID. Resolve its immutable ID and ACTIVE placement at open. Keep location/epoch as a revocable routing hint, revalidated by owner-node admission on each request; after a move, WrongOwner invalidates it and forces refresh. The data gateway contains ingress, authorization and forwarder; routing is a control-plane component; grey components execute in the node/lane processes, not the gateway. Existing FFI code is solid; lane deployment remains proposed. Carry operation IR with compiled SQL so the landed classifier can be reused without parsing SQL. The existing router interface takes only tenantId; database-aware selection/context and remote forwarding must be added. Swapping location resolution uses the landed seam without rewriting Storage; the present client API does not already support selecting arbitrary databases.',
             'Select the database by name or ID. Check permission and resolve placement when it opens. The owner checks epoch and state on each request. Refresh a stale location after a move. Grey components run on the hosting node. Pass operation IR to the installed classifier. Add database selection to the existing tenant-only router and client context.',scope='data',parent='planes-containers')


def flow(id, title, entries, plain, ste, parent):
    # Entries are ordered (relationship, direction, label). Derive exact members and static edges.
    edges = list(dict.fromkeys(r for r, _, _ in entries))
    members = list(dict.fromkeys(e for r in edges for e in [relationships[r]['sourceId'],relationships[r]['targetId']]))
    scenario = dict(kind='sequence',id=id+'-sequence',children=[
        dict(kind='interaction',id=f'{id}-s{n}',relationshipId=r,direction=direction,label=label)
        for n,(r,direction,label) in enumerate(entries,1)])
    return diagram(id,'dynamic',title,members,edges,plain,ste,parent=parent,scenario=scenario)


F = 'forward'
R = 'reverse'
flow('flow-create-database','Create database: provision then publish',[
    ('cp-request',F,'Create database for tenant, with operation ID'),
    ('cp-database',F,'Authorize and register requested database name'),
    ('cp-databases-sql',F,'Reserve immutable database ID and unique name; persist PROVISIONING operation'),
    ('cp-provision',F,'Start idempotent provisioning for database ID'),
    ('cp-placement',F,'Control plane selects VM (placement policy OPEN); reserve non-routable intent'),
    ('cp-create-file',F,'Instruct selected VM agent (process) to provision file/schema under generation token'),
    ('cp-create-file',R,'VM agent (process) confirms verified file readiness'),
    ('cp-placement',F,'CAS operation generation: publish ACTIVE owner and initial epoch atomically'),
    ('cp-provision',R,'Return successful operation state'),
    ('cp-database',R,'Return database ID and operation status'),
    ('cp-request',R,'Report ACTIVE database through management endpoint'),
], 'Success scenario. Only after the selected node verifies file/schema creation may the placement become ACTIVE. Identity and operation records are durable before file creation. A retry uses the same operation ID; a crash after creation is reconciled rather than creating a second file. See the separate provisioning-failure scenario.',
     'Record the database and operation before creating the file. Verify the file. Publish the active placement in one metadata transaction. Return the database ID.', 'planes-control-components')
flow('flow-create-failed','Create database — alternative: provisioning fails',[
    ('cp-provision',F,'Resume durable PROVISIONING operation'),
    ('cp-create-file',F,'Attempt file/schema creation'),
    ('cp-create-file',R,'Creation or verification fails; no readiness acknowledgement'),
    ('cp-placement',F,'Keep placement non-routable; record cleanup-required operation'),
    ('cp-create-file',F,'Retry idempotent removal of partial file and sidecars'),
    ('cp-create-file',R,'Acknowledge no partial file remains'),
    ('cp-placement',F,'Record FAILED operation; release reserved name only after cleanup'),
    ('cp-provision',R,'Return failed operation status'),
], 'Named failure alternative, not a second branch of the success tree. If cleanup cannot be confirmed, retain cleanup-required state and the name reservation. No partial file is published or silently orphaned. Durable operation history remains for inspection.',
     'Do not publish an unverified file. Record the failure. Remove partial files. Keep the name reserved until cleanup is confirmed.', 'planes-control-components')
flow('flow-query-right-node','Query: resolve database and execute on its owner',[
    ('dp-request',F,'Session selects one tenant database by name or ID'),
    ('dp-auth',F,'Validate request; obtain online one-request authorization for selected database'),
    ('dp-auth-sql',F,'Verify database belongs to trusted tenant and grants allow access'),
    ('dp-auth-sql',R,'Return authorized immutable database ID'),
    ('dp-route',F,'Resolve location at open for this database'),
    ('dp-placement',F,'Control-plane routing reads authoritative ACTIVE VM and epoch'),
    ('dp-placement',R,'Return node B and current epoch; client need not know node B'),
    ('dp-route',R,'Return authorized owner VM and epoch to gateway; lookup ends here'),
    ('dp-forward',F,'Invoke local forwarder with route, single-use authority decision and request identity'),
    ('dp-owner',F,'Forward tenant/database IDs, epoch, plan IR, SQL/binds and deadline to node B'),
    ('dp-classify',F,'Check one-request decision, local ACTIVE epoch and durable fence; send trusted IR to lane'),
    ('dp-execute',F,'Choose the execution path from the operation IR'),
    ('dp-deadline',F,'Apply execution deadline policy'),
    ('dp-files',F,'Execute against this database file'),
    ('dp-files',R,'Return rows after statement settles'),
    ('dp-execute',R,'Return completed execution result'),
    ('dp-classify',R,'Return lane result to node admission'),
    ('dp-owner',R,'Return rows to forwarding gateway'),
    ('dp-forward',R,'Return rows locally to gateway authorization; no router in result path'),
    ('dp-auth',R,'Return result to query ingress'),
    ('dp-request',R,'Return rows to the same client session'),
], 'Success scenario. A later request in the same session can select another database on node A through the same URL. Authorization is per selected database. Statements carry operation IR as well as SQL/binds. Unbounded-read deadline failure and stale-owner failure are separate named scenarios; no timing or throughput measurement is claimed.',
     'Use the same session and endpoint for another database. Check each selected database. Resolve its owner and execute there. Return the result through the same path.', 'planes-data-components')
flow('flow-query-stale','Query — alternative: moved database refreshes placement',[
    ('dp-owner',F,'Send request with old epoch to former owner'),
    ('dp-owner',R,'Reject before executing: WrongOwner / stale epoch'),
    ('dp-forward',R,'Invalidate cached database location'),
    ('dp-route',F,'Gateway authorization repeats routing RPC for this database'),
    ('dp-placement',F,'Router reads authoritative ACTIVE database VM and epoch'),
    ('dp-placement',R,'Return new owner and epoch after completed move'),
    ('dp-route',R,'Return refreshed route to gateway authorization'),
    ('dp-forward',F,'Reopen database location at new owner'),
    ('dp-owner',F,'Retry safely with same request ID at new owner; no client node change'),
    ('dp-owner',R,'Return result or recorded per-database mutation outcome'),
], 'Named stale-route alternative. Reject before execution. The illustrated refresh finds a completed move; MOVING/SUSPENDED/DROPPING or unavailable metadata instead returns an unavailable error and does not run SQL. A prior ambiguous mutation result requires request-ID deduplication committed in that database; blind replay is forbidden. Request-ID state moves with the database.',
     'Reject stale placement before execution. Read the new active placement. Use the same request ID. Do not blindly retry a mutation if its result is unknown.', 'planes-data-components')
flow('flow-query-deadline','Query — alternative: long read reaches deadline',[
    ('dp-execute',F,'Classified unbounded read uses nonblocking FFI step'),
    ('dp-deadline',F,'Apply remaining read deadline'),
    ('dp-interrupt',F,'Deadline expires: interrupt the read'),
    ('dp-interrupt',R,'Confirm step settled before connection reuse'),
    ('dp-execute',R,'Return typed deadline failure'),
], 'Named deadline alternative reuses landed FFI cancellation behavior. This scenario assumes interruption settles. If it does not, the proposed watchdog asks the lane supervisor to kill the process and waits for exit; all that lane’s in-flight requests fail, and database fences are not acknowledged early. This is a policy, not a measured latency guarantee.',
     'Interrupt the long read at its deadline. Wait for its execution to finish. Return a deadline error. If the process does not stop, ask the supervisor to kill it and confirm exit.', 'planes-data-components')
flow('flow-move-database','Move database: fence, copy, then flip placement',[
    ('cp-lifecycle',F,'Request move of one database A → B with operation ID'),
    ('cp-operation',F,'CAS ACTIVE → MOVING; persist source A, target B and next epoch; block new opens'),
    ('cp-node',F,'Fence this database on source A; stop all new admissions including existing sessions'),
    ('cp-node',F,'Settle requests; checkpoint with controlled maintenance connection; close it and all remaining handles'),
    ('cp-node',R,'Source A confirms durable fence and no live database handles / writers'),
    ('cp-node',F,'Instruct source VM agent (process) to copy out to target VM agent (process); target receives and verifies snapshot / request-ID state'),
    ('cp-node',R,'Target VM agent (process) confirms verified staged data and readiness; source agent stays fenced'),
    ('cp-operation',F,'Transaction CAS MOVING operation: flip owner A → B, increment epoch and publish ACTIVE'),
    ('cp-node',F,'Activate target B only after reading new ACTIVE owner / epoch; old epoch stays rejected'),
    ('cp-node',R,'Target B accepts current-epoch access'),
    ('cp-node',F,'Remove fenced source copy and sidecars; reconcile cleanup if needed'),
    ('cp-operation',F,'Record move complete; keep epoch and operation history'),
    ('cp-lifecycle',R,'Report completed move; stale routing will refresh at open'),
], 'Design choice: an offline, per-database move with temporary unavailability. The source must acknowledge a durable fence; otherwise placement does not flip. Recovery reads durable MOVING state before reopening any file. Only after requests settle, a controlled maintenance connection checkpoints, that connection and remaining handles close, and target data verifies does one Postgres transaction publish the new ACTIVE epoch. Target admits only after that commit. Source remains fenced even after restart. Cleanup follows activation; its failure never re-enables the old owner. Other databases can remain active, except for the documented shared-lane kill blast radius. Snapshot copy uses a closed/checkpointed database, not a live main-file-only copy; online WAL streaming is deferred.',
     'Stop access to this database. Drain its work and close handles on A. Verify the copied database on B. Change owner and epoch in one transaction. Activate B and remove the old copy. Keep A fenced after a restart.', 'planes-control-components')
flow('flow-move-failed','Move database — alternative: target verification fails before flip',[
    ('cp-node',R,'Source fence confirmed; staged target copy fails integrity check'),
    ('cp-operation',F,'Keep MOVING unavailable and source owner unchanged; record failed attempt'),
    ('cp-node',F,'Remove inactive target copy; require confirmed cleanup'),
    ('cp-node',R,'Confirm target cannot serve and its staged copy is removed'),
    ('cp-operation',F,'CAS rollback: publish source A ACTIVE at a fresh epoch; retain operation failure history'),
    ('cp-node',F,'Explicitly unfence A only after reading rollback ACTIVE epoch'),
    ('cp-lifecycle',R,'Return failed move status; source can serve refreshed opens'),
], 'Named pre-flip failure scenario. If source fencing cannot be confirmed, or target cleanup is not confirmed, leave unavailable and reconcile; do not guess ownership. The illustrated rollback requires the source file intact and verified and no target activation. A failure after placement commit cannot use this rollback: target remains the sole owner and the controller reconciles activation/cleanup, or runs a new fenced move.',
     'Keep the database unavailable after failed verification. Remove the inactive target copy. If the source is intact, publish a fresh epoch for A before removing its fence. After a placement commit, reconcile the new owner instead.', 'planes-control-components')
flow('flow-drop-database','Drop database: remove files then clean registry',[
    ('cp-request',F,'Drop tenant database by ID with operation ID'),
    ('cp-database',F,'Authorize database deletion'),
    ('cp-lifecycle',F,'Start durable database delete operation'),
    ('cp-operation',F,'CAS state → DROPPING; tombstone identity and block opens'),
    ('cp-node',F,'Fence owner and drain/cancel work; close read/write handles on all known copies'),
    ('cp-node',R,'Confirm no in-flight work, live handles or old copies can serve'),
    ('cp-node',F,'Instruct hosting VM agent (process) to remove file, sidecars, remnants and backups / WAL archives'),
    ('cp-node',R,'VM agent (process) confirms all database data and backups removed'),
    ('cp-operation',F,'Remove active placement and live database/name registry records; retain non-routable ID tombstone and operation audit'),
    ('cp-lifecycle',R,'Report database deletion complete'),
    ('cp-database',R,'Return completed drop status'),
    ('cp-request',R,'Acknowledge deletion; tenant and sibling databases remain'),
], 'Success scenario. Delete data before removing routing/registry evidence needed for cleanup. A database ID is never reused; its minimal tombstone is not a live database record. Deletion erases the database’s backups and archived WAL frames too; it does not report success while data cleanup remains pending. All staged copies are tracked by durable operations. Tenant deletion requires zero remaining live or deleting databases; it does not delete siblings implicitly.',
     'Block opens and stop all work for the database. Remove its files and sidecars. Remove its backups and archived WAL frames. Remove active placement and live registry records only after confirmation. Keep a non-routable tombstone. Keep sibling databases.', 'planes-control-components')
flow('flow-drop-failed','Drop database — alternative: file removal is not confirmed',[
    ('cp-node',F,'Attempt idempotent removal after fencing and drain'),
    ('cp-node',R,'Node unavailable or file/backup cleanup not confirmed'),
    ('cp-operation',F,'Retain DROPPING tombstone, placement evidence and pending cleanup operation; deny queries'),
    ('cp-lifecycle',R,'Report deletion pending, not successful; reconcile cleanup with same operation ID'),
], 'Named deletion-failure alternative. Do not clean away registry evidence and leak data. Keep the name reserved and the database unavailable until cleanup succeeds. Repeating a deletion uses the durable operation ID. The normal drop scenario resumes only after acknowledgement.',
     'Keep deletion pending if removal is not confirmed. Do not admit queries. Keep the records needed to remove all copies. Retry cleanup with the same operation ID.', 'planes-control-components')

# Illustrative deployment: uneven distribution, tenant Alpha spans nodes, node A mixes tenants.
for id, name, tech in [('dep-gateway','Gateway host','Linux host'),('dep-management','Management host','Linux host'),('dep-postgres','Metadata host','Postgres host'),('dep-a','VM A','Linux host'),('dep-b','VM B','Linux host'),('dep-c','VM C','Linux host')]:
    element(id,'deploymentNode',name,'Illustrative proposed host; not a claim about the actual droplet.',
            'This is a proposed example host. It does not describe the actual droplet.',tech=tech,environment='illustrative')
for id, name, logical, host, desc in [
    ('dep-query','Shared query endpoint','data','dep-gateway','One stable query access path for all tenant databases.'),
    ('dep-control','Management API','control','dep-management','Tenant and database lifecycle requests.'),
    ('dep-metadata','Registry / placement Postgres','metadata','dep-postgres','Database-keyed owner and epoch records; handwritten SQL.'),
    ('dep-a-agent','VM agent (process)','node','dep-a','Owns Alpha/orders, Alpha/archive and Beta/metrics (three databases).'),
    ('dep-b-agent','VM agent (process)','node','dep-b','Owns Alpha/analytics (one database).'),
    ('dep-c-agent','VM agent (process)','node','dep-c','Owns Beta/sandbox and Gamma/events (two databases).'),
    ('dep-a-lane','Node A statement lane','lanes','dep-a','Illustrative lane; per-database file handles and queues.'),
    ('dep-b-lane','Node B statement lane','lanes','dep-b','Illustrative lane for Alpha/analytics.'),
    ('dep-c-lane','Node C statement lane','lanes','dep-c','Illustrative lane; Beta and Gamma remain separate files.'),
    ('dep-alpha-orders','Alpha / orders.sqlite','files','dep-a','Tenant Alpha database orders: immutable db-a1; independent file and lock domain.'),
    ('dep-alpha-archive','Alpha / archive.sqlite','files','dep-a','Tenant Alpha database archive: immutable db-a2; independent file and lifecycle.'),
    ('dep-beta-metrics','Beta / metrics.sqlite','files','dep-a','Tenant Beta database metrics: immutable db-b1; shares node A, not a file, with Alpha.'),
    ('dep-alpha-analytics','Alpha / analytics.sqlite','files','dep-b','Tenant Alpha database analytics: immutable db-a3; same endpoint as Alpha databases on A.'),
    ('dep-beta-sandbox','Beta / sandbox.sqlite','files','dep-c','Tenant Beta database sandbox: immutable db-b2 on a different node from metrics.'),
    ('dep-gamma-events','Gamma / events.sqlite','files','dep-c','Tenant Gamma database events: immutable db-c1; separate from Beta/sandbox.'),
]:
    element(id,'instance',name,'Illustrative proposed instance. '+desc, 'This is a proposed example. '+desc,
            parent=host,instanceOf=logical,environment='illustrative')
for src,dst,model in [
    ('dep-control','dep-metadata','c-catalog'),('dep-query','dep-control','c-resolve'),
    *[( 'dep-query',f'dep-{n}-agent','c-forward') for n in 'abc'],
    *[( 'dep-control',f'dep-{n}-agent','c-lifecycle') for n in 'abc'],
    *[( f'dep-{n}-agent',f'dep-{n}-lane','c-lane') for n in 'abc'],
    ('dep-a-agent','dep-alpha-orders','c-files'),('dep-a-agent','dep-alpha-archive','c-files'),
    ('dep-a-agent','dep-beta-metrics','c-files'),('dep-b-agent','dep-alpha-analytics','c-files'),
    ('dep-c-agent','dep-beta-sandbox','c-files'),('dep-c-agent','dep-gamma-events','c-files'),
]:
    id=f'{src}-to-{dst}'
    rel(id,src,dst,relationships[model]['label'],relationships[model]['technology'],modelRelationshipId=model)
    edge_numbers[id] = len([r for r in relationships if r.startswith('dep-')])
deployment = diagram('planes-deployment','deployment','Deployment: uneven database placement (illustrative)',
                     [e for e in elements if e.startswith('dep-')],[r for r in relationships if r.startswith('dep-')],
                     'Proposed illustrative deployment, not a claim about the actual droplet. Three nodes hold 3, 1 and 2 databases. Alpha owns orders/archive on A and analytics on B; node A hosts both Alpha and Beta. Beta also spans A and C. A registered tenant Delta can have zero databases and therefore no file instance. Counts are examples, not capacity limits or measurements. Postgres stores metadata, not customer SQLite data. All instances are planned even when they reuse landed FFI code.',
                     'This deployment is illustrative. It is not a claim about the actual droplet. Put three database files on A, one on B and two on C. Alpha uses A and B. A holds separate Alpha and Beta files. Delta can have no database. These counts are examples.',parent='planes-containers',environment='illustrative')

# In-context navigation. Dynamic and deployment views use related links, not invalid detail links.
context['nodes'][2]['detail'] = dict(diagramId='planes-containers')
for n in containers['nodes']:
    if n['elementId'] in ['control','data']:
        n['detail'] = dict(diagramId='planes-'+n['elementId']+'-components')
for d in b['diagrams']:
    if d['kind'] == 'dynamic':
        siblings=[x for x in b['diagrams'] if x['kind']=='dynamic' and x['parent']==d['parent'] and x['id']!=d['id']]
        d['related']=[dict(diagramId=x['id'],label=x['title']) for x in siblings]
for d in [context,containers,cp,dp]:
    targets=[x for x in b['diagrams'] if x.get('parent',{}).get('diagramId')==d['id']]
    d['related']=[dict(diagramId=x['id'],label=x['title']) for x in targets]

# Compact card summaries; full explanations remain in the documents above.
summaries = {
    'platform': ('Database platform', 'Control plane owns routing and placement. Data plane consults it and forwards to VM agents.'),
    'backups': ('Database backup store', 'Verified snapshots per database ID and generation. Optional WAL frames are future work.'),
    'control': ('Control plane', 'Own routing, registry and VM placement decisions; instruct VM agents for all tenant-file work.'),
    'data': ('Data plane', 'One query endpoint. Consult control-plane routing, forward to VM agent, return results.'),
    'metadata': ('Postgres metadata', 'Tenants, databases, placement epochs and durable operations. Hand-written SQL; no ORM.'),
    'node': ('VM agent (process)', 'Sole VM tenant-file boundary. Execute queries and lifecycle instructions; report health, size and usage.'),
    'lanes': ('Statement lanes', 'Proposed child processes. Per-database handles, queues and request-ID state; landed FFI code.'),
    'files': ('Database SQLite files', 'One file and WAL/SHM set per database. Separate transactions and writer locks.'),
    'management': ('Management ingress', 'Authenticate administrators. Check permissions. Record idempotent operations and return status.'),
    'tenant-registry': ('Tenant registry', 'Create, configure and inspect tenants. Drop only after all owned databases are deleted.'),
    'database-registry': ('Database registry', 'Tenant-scoped database CRUD, configure and inspect. Immutable IDs; names are not paths.'),
    'provisioner': ('Database provisioner', 'Choose VM (placement policy OPEN). Instruct agent to provision; register placement after confirmation.'),
    'lifecycle': ('Database lifecycle', 'Move, suspend/resume, backup/restore and delete. Fence and drain before changing ownership.'),
    'ingress': ('Query ingress', 'Select any authorized database by name/ID through the same endpoint and session; return results.'),
    'authorization': ('Session + database auth', 'Derive trusted tenant identity. Verify database ownership and grants for each selection.'),
    'placement-router': ('Control-plane routing', 'Authority for (tenant, database) → VM / epoch. Gateway consults it; moves invalidate cached routes.'),
    'forwarder': ('Owner-node forwarder', 'Forward trusted tenant/database IDs, epoch, request ID, operation IR, SQL/binds and deadline.'),
    'node-admission': ('Admission + supervisor', 'Check database state/epoch per request. Gate, drain and close handles before acknowledging a fence.'),
    'classifier': ('Statement classifier', 'Landed operation-IR classifier: bounded blocking path or unbounded nonblocking read.'),
    'execution': ('FFI statement executor', 'Landed scoped libsqlite3 execution. Reused in proposed lanes; no separate library container.'),
    'deadline': ('Deadline / interrupt', 'Landed interrupt and settlement. Lane watchdog escalation and database-drain integration are proposed.'),
}
for id, (name, summary) in summaries.items():
    elements[id]['name'] = name
    elements[id]['description'] = summary
for id, tech in {
    'control':'Proposed Deno management service', 'data':'Proposed Deno query service',
    'node':'Proposed Deno node process', 'lanes':'Proposed Deno child processes / FFI',
    'metadata':'Postgres; hand-written SQL', 'database-registry':'Proposed logic / Postgres SQL',
    'tenant-registry':'Proposed logic / Postgres SQL', 'provisioner':'Proposed reconciler / internal RPC',
    'lifecycle':'Proposed durable state machine', 'authorization':'Proposed session/database policy',
    'placement-router':'Existing seam; proposed database router', 'forwarder':'Proposed authenticated RPC',
    'node-admission':'Proposed node gates / supervisor', 'classifier':'Existing TypeScript / operation IR',
    'execution':'Existing Deno FFI / libsqlite3', 'deadline':'Existing interrupt; proposed watchdog',
}.items():
    elements[id]['technology'] = tech
for d in [context, containers, cp, dp]:
    d['layout']['direction'] = 'LR'

# Supplemental lifecycle policies: explicit behavior without claiming implementation.
doc('doc-lifecycle-policies','Suspend, resume, backup and restore',
    'Suspend uses the same per-database gate and drain as move; SUSPENDED blocks opens and existing-session admissions. Resume requires verified file readiness, ACTIVE state and a fresh epoch. Backup takes a consistent per-database SQLite backup with an identity/generation manifest and verification; it does not copy a live main file alone. Restore stages and verifies a backup before a fenced, drained generation swap and fresh-epoch activation. IDs/authorization must still match; restoring data cannot resurrect deleted grants. Operations use durable IDs and state transitions, never a distributed transaction across SQL and files. This design chooses temporary unavailability and reconciliation over unproven online relocation or failover.',
    'Suspend one database by stopping admission and draining work. Resume it only when its file is verified. Make and verify a consistent database backup. For restore, stop access and verify staged data before replacing the generation. Change the epoch before resuming access. Keep operation records so work can resume after a crash.', ['src-brief','src-lanes'])
elements['lifecycle']['docIds'].append('doc-lifecycle-policies')
cp['docIds'].append('doc-lifecycle-policies')

# Addendum 1 overrides the original brief where routing/process authority changed.
elements['database-registry']['tags'] = ['external']
cp['edges'][-1]['displayNumber'] = 12
AGENT = ('Every hosting VM runs a daemon labelled VM agent (process), software, not AI. '
         'It is the sole last-mile tenant-file access boundary on that VM. Outside services never access its disk. '
         'Its private managed statement-lane workers execute only behind this agent boundary; they expose no independent file or RPC authority. '
         'On instruction the agent executes queries and returns results, provisions, opens/closes, suspends/resumes, snapshots/backs up, '
         'copies out/receives moves, restores and deletes files, and reports health, size and usage to the control plane.')
ROUTING = ('The control plane owns routing logic and the placement registry: (tenant, database) resolves to a hosting VM and epoch. '
           'The data plane consults that authority; a cached route is only a hint. Moves invalidate stale routes and agent admission rejects stale epochs. '
           'The topology is n SQLite files across m VMs, not one VM per database or tenant. '
           'Placement is a control-plane decision, but how it selects a VM is an OPEN QUESTION: no placement policy or balancing algorithm is solved here. '
           'Creation records active placement only after the selected VM agent confirms provisioning; the registry is then the source of truth.')
STE_AGENT = ('Each VM runs VM agent (process). This is software, not AI. Only this agent and its private execution workers can access tenant files. '
             'Send all query and lifecycle work to the agent. It returns results and reports health, size and usage. Never access VM disk from another service.')
STE_ROUTING = ('The control plane owns routing and placement records. Ask it which VM holds a tenant database. '
               'Invalidate cached routes after moves. Distribute n SQLite files across m VMs. '
               'The control plane selects the VM, but the selection policy is an OPEN QUESTION. Record active placement only after the agent confirms creation.')
replacements = {
    'node': (AGENT, STE_AGENT),
    'control': (ROUTING + ' It instructs VM agents for all file work.', STE_ROUTING + ' Send file instructions to VM agents.'),
    'data': ('Authenticate and authorize the selected database through the control plane; consult control-plane routing and forward queries to the selected VM agent (process). Return agent results. Never read placement metadata or VM files directly.',
             'Check permission through the control plane. Ask its router for the VM. Send the query to that VM agent (process). Return its results.'),
    'placement-router': (ROUTING + ' The landed TenantStorageRouter seam is tenant-only; database-aware ControlPlaneRouter and remote routing remain planned.', STE_ROUTING + ' Extend the existing tenant-only router for database selection.'),
    'provisioner': ('The control plane selects a VM; placement policy is an OPEN QUESTION. Reserve identity and non-routable operation intent, instruct its VM agent (process) to create and verify a file, then publish placement only after agent confirmation. Reconcile partial files through the agent.',
                    'Select a VM in the control plane. The selection policy is open. Ask its agent to create and verify the file. Record active placement after confirmation.'),
    'planes-containers': (ROUTING + ' ' + AGENT, STE_ROUTING + ' ' + STE_AGENT),
    'planes-control-components': (ROUTING + ' Tenant and database registries manage CRUD using Postgres hand-written SQL, no ORM. Lifecycle controllers instruct VM agents for moves, suspend/resume, backup/restore and deletion; they never manipulate disk directly.',
                                 STE_ROUTING + ' Use hand-written SQL in Postgres. Instruct VM agents to do all lifecycle file work.'),
    'planes-data-components': ('Ingress authenticates and authorizes the selected database through the control-plane registry, consults control-plane routing at open, then forwards to the VM agent (process). The router belongs to the control plane, not the gateway; database registry details are in the control-plane view. The gateway contains ingress, authorization and forwarding only. ' + AGENT + ' Carry operation IR with SQL/binds to reuse the landed classifier. Database-aware API selection and remote routing are planned.',
                              'The gateway contains ingress, authorization and forwarding. Routing and registry logic belong to the control plane. Ask it for the VM, then send the query to its agent. ' + STE_AGENT),
}
for id, (plain, ste) in replacements.items():
    d = next(d for d in b['documents'] if d['id'] == 'doc-' + id)
    d.update(plain=plain + '\n\n' + SHIFT)
    d['ste']['text'] = ste + '\n\n' + STE_SHIFT
    if id in elements:
        provenance[id] = plain
for d in b['documents']:
    id = d['id'].removeprefix('doc-')
    if id in elements:
        d['title'] = elements[id]['name']
# Append explicit boundary/authority notes to every explanation, including inherited flow policies.
for d in b['documents']:
    for note in [ROUTING, AGENT]:
        if note not in d['plain']:
            d['plain'] += '\n\n' + note
    for note in [STE_ROUTING, STE_AGENT]:
        if note not in d['ste']['text']:
            d['ste']['text'] += '\n\n' + note
    d['sourceIds'] = list(dict.fromkeys(d['sourceIds'] + ['src-addendum']))
for obj in b['model']['elements'] + b['model']['relationships'] + b['diagrams']:
    obj['sourceIds'] = list(dict.fromkeys(obj['sourceIds'] + ['src-addendum']))
containers['groups'][0]['label'] = 'One of m VMs — VM agent boundary and its private workers / files'
dp['groups'] = [dict(id='g-cp-authority', label='Control plane — routing authority',
                     memberIds=['placement-router','database-registry','metadata'], status='planned'),
                dict(id='g-agent-execution', label='VM agent (process) — private execution internals',
                     memberIds=['node-admission','classifier','execution','deadline','files'], status='planned')]
cp['title'] = 'Control plane: routing authority, placement and lifecycle'
containers['title'] = 'Containers: control-plane routing → VM agent (process) → n files on m VMs'
elements['lanes']['description'] = 'Private agent-managed execution workers; no independent external endpoint or file authority. Landed FFI code inside proposed lanes.'
elements['node-admission']['name'] = 'VM agent (process) — admission'
elements['placement-router']['plannedNotes'] = ['Tenant-only seam is landed; control-plane database routing and remote execution are planned.']


# DP-01 repair: routing, local admission and trust contracts are explicit proposal decisions.
def extend(ids, plain, ste):
    for id in ids:
        document = next(d for d in b['documents'] if d['id'] == 'doc-' + id)
        document['plain'] += '\n\n' + plain
        document['ste']['text'] += '\n\n' + ste
        if id in elements:
            provenance[id] += ' ' + plain

AVAILABILITY = """Chosen availability policy: fail closed for ALL new admissions when control-plane authority cannot be confirmed. There is no offline lease or heartbeat authorization. Decision issuance is the admission point; no decision prefetch or reuse is permitted. Every request, including an existing session, needs an online one-request admission decision from the authority, bound to database ID, owner epoch/generation, principal, permission, request ID and a deadline of at most 30 seconds. Agent checks that decision and its durable local ACTIVE epoch/fence. A decision is consumed once; it cannot open an offline session. Revocation denies subsequent decisions; a decision already issued may execute until its deadline, and already-running work settles or is cancelled by its deadline. Lifecycle fences override outstanding decisions locally.

| State / event | Admission / recovery |
| --- | --- |
| Authority reachable, database ACTIVE | Online authorization and current epoch plus durable local ACTIVE state required for each request. |
| Authority unavailable, running agent | Reject new requests, including existing sessions. Already admitted work settles within its deadline; no transfer. |
| MOVING, SUSPENDED, DROPPING or durable local fence | Reject admission even with a previously issued decision. No ownership transfer without confirmed source fencing. |
| Agent restart | Keep closed. Terminate and confirm exit of orphan workers before opening files. Reconcile committed owner/epoch/generation and durable fences online; unavailable authority means stay closed. |
| Controller crash after placement commit, before activation ACK | Target remains sole committed owner; it stays closed until idempotent activation reconciles that commit. Source stays fenced. Never roll back to stale ownership. |

Serialize lifecycle operations per database. Every command carries database ID, operation ID, expected owner epoch and generation, and intended transition. Persist accepted transition/fence and its acknowledgement before replying. Replay returns recorded status; reject superseded activation, unfence and delete commands. Cleanup names the exact old generation and operation: a delayed A-to-B delete cannot delete A's later active generation after B-to-A. Recheck these preconditions before filesystem effects, not only on receipt."""
STE_AVAILABILITY = """Policy: reject every new request if the control plane cannot confirm authority. Do this for existing sessions too. There is no offline lease or heartbeat authority. Admission occurs when the decision is issued. Do not prefetch or reuse decisions. For each request, obtain one online admission decision. Bind it to the database ID, owner epoch, generation, principal, permission, request ID and a deadline no more than 30 seconds away. Use it once. Check the local durable ACTIVE state and fence too. Reject new decisions after permission revocation. An issued decision can run until its deadline. A local fence blocks it immediately.

| State / event | Required action |
| --- | --- |
| ACTIVE and authority available | Check online permission and epoch, then local state, for each request. |
| Authority unavailable | Reject new requests. Let admitted work settle by its deadline. Do not move ownership. |
| Fenced or inactive | Reject requests, including decisions issued before the fence. |
| Agent restart | Keep files closed. Stop orphan workers and confirm exit. Reconcile ownership and durable fences online before opening files. |
| Controller stops after owner commit | Keep the source fenced. Reconcile activation on the committed target. Do not restore the old owner. |

Serialize lifecycle commands for each database. Each command must name the database, operation, expected epoch, generation and intended transition. Store the transition and acknowledgement durably before replying. Reject obsolete activation, unfence and delete commands. Before changing files, check the operation and exact generation again. A delayed cleanup must not delete a later active generation."""
extend(['node-admission','placement-router','flow-move-database','flow-move-failed'], AVAILABILITY, STE_AVAILABILITY)
TRUST = ('Trust boundary: agents trust authenticated gateway service assertions for execute-only operations, including the authority-issued one-request decision described in the admission contract. Customers cannot issue these assertions. Gateway identity cannot provision, unfence, move or delete. Only the control-plane lifecycle identity can issue lifecycle commands, subject to durable operation/epoch/generation checks. Agent-to-agent snapshot transfer credentials authorize only the named operation, source, target, database and generation, expire at that operation deadline and cannot authorize execution or activation. '
         'Trusted gateway server code validates the supported document CRUD/query request, then derives matching operation IR and parameterized SQL/binds from that same request. No customer SQL or customer-supplied IR is accepted. Enforce this at ingress before classification; agent accepts only the authenticated compiler/gateway identity and scoped database operation. Disable SQL extension loading and file-access features, ATTACH, arbitrary PRAGMA and path-bearing SQL; the worker opens only the agent-selected database. The landed classifier is not a security validator. This is a proposed boundary, not an implemented security subsystem.')
STE_TRUST = ('Agents accept execute assertions only from the authenticated gateway service. Require the online one-request authority decision. The gateway cannot create files, unfence, move or delete. Only the control-plane lifecycle identity can send lifecycle commands. Check their operation, epoch and generation. Limit transfer credentials to one operation, source, target, database, generation and operation deadline. They cannot activate a database. '
             'Accept only supported document CRUD and query requests. Trusted gateway code validates each request and generates matching IR and parameterized SQL. Reject customer SQL and IR. Disable extension loading, file access, ATTACH, arbitrary PRAGMA and SQL paths. Workers open only the database selected by the agent. The classifier does not validate security. This boundary is proposed.')
extend(['authorization','ingress','forwarder','node','node-admission'], TRUST, STE_TRUST)
BUDGETS = ('Retain the lanes proposal bounded pool, scheduler and handle-cache policies, keyed by database with aggregate tenant and VM ceilings. Bound queue count and bytes, concurrent reads, one writer per database, open handles and lane count. Reject or backpressure excess admission; fairly schedule databases within tenant budgets so many databases cannot consume the VM. Evict idle handles only after transactions settle. Require disk plus WAL/snapshot headroom and bounded backup/move I/O reservations; throttle or reject when exhausted. Reserve supervisor/control capacity for fencing and cancellation under execution saturation. Quotas are accounting, not a tenant-wide execution lock. 0..N is cardinality, not unlimited capacity. Logical/file isolation is provided; VM/process failure and resource contention remain shared. Numeric limits and fairness weights remain OPEN configuration choices.')
STE_BUDGETS = ('Keep bounded lanes, queues, handles and scheduling. Apply database limits and total tenant and VM limits. Bound queue bytes, reads, writers and I/O. Permit one writer per database. Reject or slow excess work. Schedule fairly across tenant budgets. Close idle handles after transactions finish. Reserve disk space for WAL, snapshots and moves. Reserve supervisor capacity to fence and cancel when execution is full. Tenant budgets do not require a tenant execution lock. Many databases do not mean unlimited capacity. Files are separate; VM failure and resource contention are shared. Numeric limits and scheduling weights remain OPEN.')
extend(['lanes','node-admission','deadline'], BUDGETS, STE_BUDGETS)
PLACEMENT = ('Placement algorithm and weights remain OPEN, but eligibility requires fresh authenticated agent health/readiness, compatible software/schema versions, available disk including WAL/snapshot/move headroom, resource ceilings and requested locality constraints. Reserve capacity transactionally for concurrent creates and staged moves; release reservations only after confirmed cleanup. Reject placement when no VM is eligible. Agents report health, database sizes, usage and reservations to control-plane metadata; reports carry agent incarnation and monotonic sequence. Stale/missing reports make a VM ineligible for new placements; they never prove an old writer fenced. Reporting interval and freshness threshold remain OPEN configuration decisions.')
STE_PLACEMENT = ('The placement algorithm and weights remain OPEN. Require fresh health and readiness reports, compatible software and schema, sufficient disk and WAL/snapshot/move space, resource limits and locality. Reserve capacity for creates and staged moves. Release it after cleanup is confirmed. Reject placement if no VM is eligible. Agents report health, size and usage with an incarnation and sequence number. Stale reports block new placement. They do not prove a writer stopped. Report intervals and freshness limits remain OPEN.')
extend(['provisioner','metadata','planes-control-components'], PLACEMENT, STE_PLACEMENT)
DEFER6 = ('Review tweak 6 DEFERRED: end-to-end backup publication, VM-loss recovery and replacement backup-deletion executor scenarios need a separate recovery design that chooses storage credentials, durable manifest ownership and acceptable isolation evidence; DP-01 does not choose those mechanisms. RPO/RTO policy remains OPEN; WAL streaming is deferred. Until then only verified identity/generation/checksum/format-compatible completed backups can be candidates for restore, not a promise of recoverability or continuous protection. Losing a VM makes every hosted database unavailable. Detection is not fencing proof; no replacement writer may activate without explicit operator-verified isolation/proof of fencing. If the former agent is permanently lost, keep deletion DROPPING and the name reserved: no agent or object-store deletion confirmation is invented. Full backup completion manifests and replacement cleanup remain unimplemented design work, not a successful recovery path.')
STE_DEFER6 = ('Review tweak 6 is DEFERRED to a recovery design. It must choose storage credentials, manifest ownership, fencing evidence and a replacement cleanup executor before adding complete flows. RPO and RTO remain OPEN. WAL streaming is deferred. Consider only completed, verified backups with database and generation IDs, checksum and compatible format for restore. This does not promise recovery or continuous protection. If a VM is lost, all its databases are unavailable. Failure detection does not prove fencing. Require operator-verified isolation before any replacement writer activates. If the old agent is permanently lost, keep deletion DROPPING and reserve the name. Backup publication and replacement cleanup need further design.')
extend(['backups','lifecycle-policies','planes-control-components','flow-drop-database','flow-drop-failed'], DEFER6, STE_DEFER6)
DEFER7 = ('Review tweak 7 instance companion DEFERRED: source/target instance-level move and failure diagrams will accompany the recovery design so transfer credentials and isolation evidence can be reviewed together. The current dynamic participant node denotes the logical VM agent type, not a single physical VM; step labels identify source A and target B. Proposed transfer is authenticated direct agent-to-agent snapshot streaming scoped to the move operation, not snapshot bytes through the controller. Ordering correction APPLIED: settle requests, checkpoint through a controlled maintenance connection, close that connection and all remaining handles, then durably acknowledge quiescence and copy. No fence acknowledgement precedes worker exit/maintenance completion.')
STE_DEFER7 = ('Review tweak 7 instance companion is DEFERRED to the recovery design so transfer credentials and fencing evidence are reviewed together. The node participant is a logical agent type. A and B in the steps are different VMs. Proposed snapshot bytes stream directly from source agent to target agent with a move-scoped credential. The controller sends instructions only. Corrected order: settle requests; checkpoint with a controlled maintenance connection; close it and all other handles; durably acknowledge quiescence; copy. Confirm worker exit and maintenance completion before acknowledgement.')
extend(['flow-move-database','flow-move-failed'], DEFER7, STE_DEFER7)
PATH = 'Authorization calls the control-plane router and receives its route on the reverse lookup RPC. It then calls the forwarder locally. Query rows return agent to forwarder to authorization to ingress/client; they never pass through the router. Stale routing repeats the gateway routing RPC before the router reads metadata.'
STE_PATH = 'Ask the router for a route. Receive the route on the lookup response. Call the gateway forwarder locally. Return rows from agent to forwarder to authorization to ingress and client. Do not send rows through the router. For a stale route, repeat the gateway routing RPC before reading placement.'
extend(['flow-query-right-node','flow-query-stale','planes-data-components','forwarder'], PATH, STE_PATH)
for rid, label in [('c-lifecycle','Lifecycle identity only: epoch/generation-checked file commands'), ('c-forward','Gateway execute-only identity + one-request authority decision')]:
    relationships[rid]['label'] = label
    provenance[rid] = label + '. ' + TRUST
rel('cp-health','node','provisioner','Report health, size, usage and reservations; stale is ineligible, not fenced','Authenticated control-plane reporting RPC')
edge_numbers['cp-health'] = 14
cp['edges'].append(dict(relationshipId='cp-health', displayNumber=14))
# A separate agent detail preserves the landed execution path without crowding the overview.
execution_members = ['node-admission','classifier','execution','deadline','files']
execution_edges = ['dp-classify','dp-execute','dp-deadline','dp-interrupt','dp-escalate','dp-files']
agent_detail = diagram('planes-agent-components','component','VM agent: private execution and deadlines', execution_members, execution_edges,
    AGENT + ' ' + BUDGETS, STE_AGENT + ' ' + STE_BUDGETS, scope='node', parent='planes-data-components')
dp['nodes'] = [n for n in dp['nodes'] if n['elementId'] not in ['classifier','execution','deadline','files','metadata']]
dp['edges'] = [e for e in dp['edges'] if e['relationshipId'] not in execution_edges + ['dp-placement']]
dp['groups'] = [dict(id='g-cp-authority',label='Control plane — lookup only',memberIds=['placement-router','database-registry'],status='planned'),
                dict(id='g-agent-execution',label='VM agent boundary — execute only; lifecycle identity separate',memberIds=['node-admission'],status='planned')]
dp['nodes'] = [n for n in dp['nodes'] if n['elementId'] not in ['caller','database-registry']]
dp['edges'] = [e for e in dp['edges'] if e['relationshipId'] not in ['dp-request','dp-auth-sql']]
dp['groups'][0]['memberIds'] = ['placement-router']
dp['groups'][1]['label'] = 'VM agent — scoped service identities'
dp['layout']['direction'] = 'LR'
for rid, label in [('dp-auth','Validate request + permission'), ('dp-route','Online authority lookup'), ('dp-forward','Local authorized dispatch'), ('dp-owner','Execute-only RPC / result')]:
    relationships[rid]['label'] = label
    provenance[rid] = label + '. ' + PATH
dp['related'].append(dict(diagramId=agent_detail['id'],label=agent_detail['title']))
REPAIR_NOTES = [('DP6 / tweak 1 — applied', PATH), ('DP6 / DP7 / tweak 2 — applied', AVAILABILITY),
                ('DP6 / tweak 3 — applied', TRUST), ('DP9 / tweak 4 — applied', BUDGETS),
                ('A3 / tweak 5 — applied', PLACEMENT), ('DP8 / DP10 / tweak 6 — deferred', DEFER6),
                ('DP7 / tweak 7 — partial, companion deferred', DEFER7),
                ('DP1 / DP9 / DP10 / tweak 8 — applied', 'The data overview links to a separate agent execution detail. The lanes lease/heartbeat contract is not specified or retained here; the replacement is online per-request authority with fail-closed admission. Bounded pool/scheduler/handle policies are retained with aggregate tenant and VM budgets. Existing/mixed/planned markings remain unchanged. The logical move capture is not an instance participant diagram; see explicit tweak 7 deferral.')]
for obj in b['documents'] + b['model']['elements'] + b['model']['relationships'] + b['diagrams']:
    obj['sourceIds'] = list(dict.fromkeys(obj['sourceIds'] + ['src-review']))

for obj in b['diagrams'] + b['model']['elements']:
    document = next(d for d in b['documents'] if d['id'] == 'doc-' + obj['id'])
    document['title'] = obj.get('title', obj.get('name'))



# =====================================================================================================
# Part B — DP-02: Fable's modifications M1–M8 (docs/architecture/scale-out-comparison-fable.md §4).
# Everything above is DP-01's generator; everything below changes only what an M-number names.
# =====================================================================================================
source('src-dp02-brief', 'agent-work/items/dp-02-fable-planes/brief.md', 'DP-02 brief: build the Fable revision as its own dataset', DP02_BRIEF)
source('src-comparison', 'docs/architecture/scale-out-comparison-fable.md', 'Fable comparison and pick; modifications M1–M8 (§4)')
source('src-planes', 'pages/public/architectures/mansio-planes-proposal.json', 'Base dataset this revision patches (DP-01, landed eb5df16)')
source('src-planes-inventory', 'pages/content/mansio-planes-proposal/inventory.md', 'DP-01 decisions A1–A4, DP1–DP13 and repair dispositions')
source('src-rereview', 'agent-work/items/dp-01-planes/review-astra-rereview.md', 'DP-01 re-review: accepted deferrals 6 and 7, residual nit on fresh decisions')
source('src-dp01-report', 'agent-work/items/dp-01-planes/report.md', 'DP-01 builder report')
source('src-backup', 'platform/backup.ts', 'Landed backup pipeline: VACUUM INTO on the writer queue, quick_check, SHA-256, immutable PUT, manifest, latest.json')
FABLE = ['src-dp02-brief', 'src-comparison', 'src-planes']
M = {n: f'src-comparison §4 M{n}' for n in range(1, 9)}
FABLE_SHIFT = ('Fable revision (DP-02): the planes proposal with modifications M1–M8 from docs/architecture/scale-out-comparison-fable.md '
               'applied. Everything an M-number does not name is carried over from mansio-planes-proposal unchanged. This is a proposal, not landed behavior.')
STE_FABLE_SHIFT = ('This is the Fable revision of the planes proposal. It applies modifications M1 to M8 from the comparison document. '
                   'All other parts are the same as the planes proposal. This is a proposal. It is not landed behavior.')


def fdoc(id, title, plain, ste, sources=None, terms=None):
    d = dict(id=id, title=title, plain=plain + '\n\n' + SHIFT, ste=dict(text=ste + '\n\n' + STE_SHIFT, issue='ASD-STE100 Issue 9', reviewStatus='draft'),
             termIds=terms or [], sourceIds=sources or FABLE)
    b['documents'].append(d)
    return id


def felement(id, kind, name, description, ste, parent=None, status='planned', sources=None, tech=None, tags=None, notes=None, origin=None, terms=None, **extra):
    sources = sources or FABLE
    e = dict(id=id, kind=kind, name=name, description=description, status=status, tags=tags or [], plannedNotes=notes or [],
             sourceIds=sources, docIds=[fdoc('doc-' + id, name, description, ste, sources, terms)], **extra)
    if parent:
        e['parentId'] = parent
    if tech:
        e['technology'] = tech
    b['model']['elements'].append(e)
    elements[id] = e
    provenance[id] = origin or description
    return id


def frel(id, src, dst, label, tech, sources=None, status='planned', origin=None, number=None, **extra):
    r = dict(id=id, sourceId=src, targetId=dst, label=label, technology=tech, status=status, plannedNotes=[], docIds=[], sourceIds=sources or FABLE, **extra)
    b['model']['relationships'].append(r)
    relationships[id] = r
    provenance[id] = origin or label
    prefix = id.split('-')[0] + '-'
    edge_numbers[id] = number or max([n for k, n in edge_numbers.items() if k.startswith(prefix)] + [0]) + 1
    return id


def document(id):
    return next(d for d in b['documents'] if d['id'] == 'doc-' + id)


def replace_text(ids, old_plain, new_plain, old_ste, new_ste):
    for id in ids:
        d = document(id)
        assert old_plain in d['plain'], (id, old_plain[:60])
        d['plain'] = d['plain'].replace(old_plain, new_plain)
        assert old_ste in d['ste']['text'], (id, old_ste[:60])
        d['ste']['text'] = d['ste']['text'].replace(old_ste, new_ste)
        if id in elements:
            provenance[id] = provenance[id].replace(old_plain, new_plain)


def drop_diagram(id):
    b['diagrams'][:] = [d for d in b['diagrams'] if d['id'] != id]
    b['documents'][:] = [d for d in b['documents'] if d['id'] != 'doc-' + id]


b['terms'] = [
    dict(id='term-route-grant', text='route grant', category='technicalNoun',
         definition='A time-boxed, revocable record the control-plane routing authority issues to a gateway for one database: owner VM, epoch, generation, grant version and TTL. The gateway reuses it across requests until it expires, WrongOwner invalidates it or a grant-version bump revokes it (M1).'),
    dict(id='term-default-database', text='default database', category='technicalNoun',
         definition='The reserved database named default that every tenant has; a request that names no database gets it. Each existing tenant file becomes its tenant\'s default database at migration (M4).'),
    dict(id='term-backup-manifest', text='backup manifest', category='technicalNoun',
         definition='The recorded completion record of one backup, keyed by (database ID, generation): object key, checksum, format and completion state. Only a recorded manifest makes a backup a restore candidate (M6).'),
    dict(id='term-attestation', text='isolation attestation', category='technicalNoun',
         definition='A platform operator\'s statement, backed by a recorded provider-API response, that a lost VM is powered off or network-detached. It is the precondition for activating any copy of a database the VM hosted (M5).'),
]

# ---------------------------------------------------------------- M1: route grants with a TTL -------------
GRANT = ('Route grant (M1): the routing authority resolves (tenant, database) once and issues a route grant per (gateway, database ID) bound to the owner VM, epoch and generation, '
         'the principal\'s grant version and a TTL of 30 s, the number DP-01 used as its decision deadline. The gateway forwarder caches and reuses the grant for any number of requests until it expires, '
         'WrongOwner invalidates it, or a grant-version bump revokes it; the router may push a best-effort invalidation, and the TTL bounds any missed one. The router stays authoritative but is out of the '
         'per-request path while a grant is valid: two control-plane RPCs per request become two per grant lifetime, and a control-plane outage degrades to "no new grants after the TTL" instead of stopping all traffic. '
         'Every forwarded request still carries the grant\'s epoch and generation, and the VM agent checks them against its durable local ACTIVE epoch and fence; that check, not the grant, is what prevents stale execution. '
         'A stale-route retry obtains a fresh decision bound to the new epoch; no grant from the old epoch is reused. New grants fail closed when the authority is unreachable. '
         'This is NOT the lanes lease: an owner never serves on a timer, only on durable state set by a fenced flip, so no clock enters the fencing argument. Astra\'s tweak 2 is reopened on exactly this point and needs Nick\'s word and a re-review.')
STE_GRANT = ('Route grant: the routing authority finds the owner VM for a tenant database once. It gives the gateway a route grant for that database. The grant names the owner VM, epoch, generation, grant version and a time limit of 30 seconds. '
             'The gateway reuses the grant for many requests until it expires, until WrongOwner invalidates it, or until the authority revokes it. The router is not on the path of each request while a grant is valid. '
             'Each request still carries the epoch and generation. The VM agent checks them against its durable local state and fence. That check stops stale execution. '
             'After a stale route, obtain a fresh decision for the new epoch. Do not reuse an old grant. If the authority is not reachable, do not issue new grants. This is not a lease. An owner serves only on durable state set by a fenced flip.')
felement('grant-cache', 'component', 'Route grant cache',
         'Holds route grants per (gateway, database ID): owner VM, epoch, generation, grant version, TTL. Reused until expiry, WrongOwner or revocation; never a lease.',
         STE_GRANT, parent='data', tech='Proposed in-memory cache; 30 s TTL', sources=FABLE + ['src-lanes'], terms=['term-route-grant'],
         origin='M1 route grant cache in the gateway. ' + GRANT)
document('grant-cache')['plain'] = GRANT + '\n\n' + SHIFT
frel('dp-grant', 'authorization', 'grant-cache', 'Look up, store or invalidate the route grant for this database', 'In-process call', origin='M1: grant lookup, store and invalidation are gateway-local.')
frel('dp-revoke', 'placement-router', 'grant-cache', 'Best-effort invalidation on move flip or grant-version bump; the TTL bounds a missed one', 'Authenticated control-plane push RPC (best effort)',
     origin='M1: revocation is best effort; correctness rests on the agent epoch check and the TTL, never on this push.')
for rid, label in [('dp-route', 'Route grant request: miss, expiry, revocation or WrongOwner'),
                   ('dp-forward', 'Local dispatch with a valid route grant'),
                   ('dp-owner', 'Execute-only RPC carrying grant epoch / result'),
                   ('c-resolve', 'Obtain route grants and database authorization; reuse per grant TTL'),
                   ('c-forward', 'Gateway execute-only identity + route grant (epoch, generation, TTL)')]:
    relationships[rid]['label'] = label
    relationships[rid]['sourceIds'] = list(dict.fromkeys(relationships[rid]['sourceIds'] + ['src-comparison']))
    provenance[rid] = label + '. ' + GRANT
dp['nodes'].append(dict(elementId='grant-cache'))
dp['edges'] += [dict(relationshipId='dp-grant', displayNumber=edge_numbers['dp-grant']), dict(relationshipId='dp-revoke', displayNumber=edge_numbers['dp-revoke'])]
dp['groups'][0]['label'] = 'Control plane — grant issuer, out of the per-request path'
dp['title'] = 'Data plane: route grant → forwarder → VM agent (M1)'
GRANTS_TABLE = """Chosen availability policy (M1, replaces DP-01's single-use decision per request): the routing authority issues a route grant per (gateway, database ID) bound to the owner VM, epoch and generation, the principal's grant version and a TTL of 30 s. The gateway reuses a grant for any number of requests until it expires, WrongOwner invalidates it or a grant-version bump revokes it. Every request still carries the grant's epoch and generation; the agent checks them against its durable local ACTIVE epoch and fence, and that check is what prevents stale execution. New grants fail closed when the authority is unreachable. A stale-route retry obtains a fresh decision bound to the new epoch. There is no lease and no clock in the fencing argument. Revocation takes effect within the TTL, the same 30 s bound DP-01 had.

| State / event | Admission / recovery |
| --- | --- |
| Authority reachable, database ACTIVE | A valid grant (unexpired, matching grant version) plus the agent's durable local ACTIVE state and matching epoch, for each request. |
| Authority unavailable, running agent | Existing grants serve until their TTL; no new grants. After the TTL the gateway fails closed. Already admitted work settles within its deadline; no transfer. |
| MOVING, SUSPENDED, DROPPING or durable local fence | Reject admission even with a valid grant: the local fence overrides. WrongOwner invalidates the gateway's grant. No ownership transfer without confirmed source fencing. |
| Agent restart | Keep closed. Terminate and confirm exit of orphan workers before opening files. Reconcile committed owner/epoch/generation and durable fences online; unavailable authority means stay closed. |
| Controller crash after placement commit, before activation ACK | Target remains sole committed owner; it stays closed until idempotent activation reconciles that commit. Source stays fenced. Never roll back to stale ownership. |

Serialize lifecycle operations per database. Every command carries database ID, operation ID, expected owner epoch and generation, and intended transition. Persist accepted transition/fence and its acknowledgement before replying. Replay returns recorded status; reject superseded activation, unfence and delete commands. Cleanup names the exact old generation and operation: a delayed A-to-B delete cannot delete A's later active generation after B-to-A. Recheck these preconditions before filesystem effects, not only on receipt."""
STE_GRANTS_TABLE = """Policy: the authority gives the gateway a route grant for each database. The grant names the owner VM, epoch, generation, grant version and a 30-second time limit. Reuse it for many requests until it expires, until WrongOwner invalidates it, or until the authority revokes it. Each request carries the epoch and generation. The agent checks them against its durable local state and fence. If the authority is not reachable, do not issue new grants. After a stale route, obtain a fresh decision for the new epoch. There is no lease.

| State / event | Required action |
| --- | --- |
| ACTIVE and authority available | Check the grant and its time limit, then the local state and epoch, for each request. |
| Authority unavailable | Use existing grants until they expire. Do not issue new grants. Let admitted work settle by its deadline. Do not move ownership. |
| Fenced or inactive | Reject requests, including requests with a valid grant. Invalidate the grant after WrongOwner. |
| Agent restart | Keep files closed. Stop orphan workers and confirm exit. Reconcile ownership and durable fences online before opening files. |
| Controller stops after owner commit | Keep the source fenced. Reconcile activation on the committed target. Do not restore the old owner. |

Serialize lifecycle commands for each database. Each command must name the database, operation, expected epoch, generation and intended transition. Store the transition and acknowledgement durably before replying. Reject obsolete activation, unfence and delete commands. Before changing files, check the operation and exact generation again. A delayed cleanup must not delete a later active generation."""
replace_text(['node-admission', 'placement-router', 'flow-move-database', 'flow-move-failed'], AVAILABILITY, GRANTS_TABLE, STE_AVAILABILITY, STE_GRANTS_TABLE)
replace_text(['authorization', 'ingress', 'forwarder', 'node', 'node-admission'],
             'including the authority-issued one-request decision described in the admission contract', 'including the route grant (M1) described in the admission contract; the agent verifies the grant\'s epoch and generation against its durable local state',
             'Require the online one-request authority decision.', 'Require a valid route grant. Check its epoch and generation against local state.')
replace_text(['authorization', 'ingress', 'forwarder', 'node', 'node-admission'],
             'agent accepts only the authenticated compiler/gateway identity and scoped database operation.',
             'agent accepts only the authenticated compiler/gateway identity, a scoped database operation and a grant epoch that matches its local state.',
             'Workers open only the database selected by the agent.', 'Workers open only the database selected by the agent.')
PATH2 = 'Authorization looks up the route grant for the database in the gateway grant cache. On a miss, expiry or revocation it calls the control-plane router once and stores the grant it receives on the reverse lookup RPC; on a hit it makes no control-plane call. It then calls the forwarder locally with the grant. Query rows return agent to forwarder to authorization to ingress/client; they never pass through the router. A stale route invalidates the grant and obtains a fresh decision bound to the new epoch before the retry.'
STE_PATH2 = 'Look up the route grant in the gateway cache. If there is no valid grant, ask the router once and store the grant. If there is a valid grant, do not call the control plane. Call the gateway forwarder locally with the grant. Return rows from agent to forwarder to authorization to ingress and client. Do not send rows through the router. After a stale route, invalidate the grant and obtain a fresh decision before the retry.'
replace_text(['planes-data-components', 'forwarder'], PATH, PATH2, STE_PATH, STE_PATH2)
replace_text(['data'], 'consult control-plane routing and forward queries to the selected VM agent (process).', 'obtain a route grant from control-plane routing once per grant lifetime (M1) and forward queries to the selected VM agent (process) with the grant.',
             'Ask its router for the VM.', 'Ask its router for a route grant. Reuse the grant until it expires.')
for id, (name, summary) in {
    'placement-router': ('Control-plane routing', 'Authority for (tenant, database) → VM / epoch. Issues 30 s route grants; out of the per-request path while a grant is valid.'),
    'data': ('Data plane', 'One query endpoint. Obtain a route grant once per grant lifetime, forward with the grant, return results.'),
    'forwarder': ('Owner-node forwarder', 'Forward trusted IDs, grant epoch/generation, request ID, operation IR, SQL/binds and deadline. Invalidate the grant on WrongOwner.'),
}.items():
    elements[id]['name'] = name
    elements[id]['description'] = summary
ROUTING2 = ROUTING.replace('The data plane consults that authority; a cached route is only a hint. Moves invalidate stale routes and agent admission rejects stale epochs.',
                           'The data plane holds a time-boxed route grant from that authority (M1) and reuses it until TTL, WrongOwner or revocation; agent admission rejects stale epochs, which is what makes a stale grant harmless.')
STE_ROUTING2 = STE_ROUTING.replace('Invalidate cached routes after moves.', 'Hold a route grant with a time limit. Invalidate it after WrongOwner. The agent rejects stale epochs.')
assert ROUTING2 != ROUTING and STE_ROUTING2 != STE_ROUTING
for d in b['documents']:
    d['plain'] = d['plain'].replace(ROUTING, ROUTING2)
    d['ste']['text'] = d['ste']['text'].replace(STE_ROUTING, STE_ROUTING2)
for k in list(provenance):
    provenance[k] = provenance[k].replace(ROUTING, ROUTING2)
ROUTING, STE_ROUTING = ROUTING2, STE_ROUTING2


# ---------------------------------------------------------------- M4: default database + selection surface -----
SELECTION = ('Selection surface (M4): a request names a database through a `database` option on the client (per session) or a per-call override; the name is tenant-scoped. '
             'A request that names none gets the tenant\'s reserved database `default`, so createClient(baseURL, appId, token) keeps working unchanged. The gateway resolves the name to the '
             'immutable database ID under the session\'s trusted tenant and stamps database_id into the operation IR after authorization; the classifier and the agent never see a name. '
             'Migration: every existing tenant file becomes database `default` with a minted immutable ID and a placement row (owner = the one existing node, epoch 1); the planned ControlPlaneRouter '
             'becomes resolve(databaseId) with TenantContext extended by a database. Fable\'s default for the open schema question: a database carries its own application schema fingerprint, as _platform_schema does per file today; not yet decided.')
STE_SELECTION = ('Selection: a client names a database with the `database` option for a session, or for one call. If a request names no database, use the tenant\'s database named `default`. '
                 'The existing client call does not change. The gateway converts the name to the database ID for the trusted tenant. It writes the ID into the operation IR after authorization. '
                 'The classifier and the agent never see a name. At migration, each existing tenant file becomes that tenant\'s `default` database with a new ID and a placement row. The schema question is not decided.')
extend(['ingress', 'authorization', 'placement-router', 'caller', 'database-registry'], SELECTION, STE_SELECTION)
for id in ['ingress', 'authorization', 'placement-router', 'caller', 'database-registry']:
    document(id)['termIds'] = list(dict.fromkeys(document(id)['termIds'] + ['term-default-database']))
elements['ingress']['description'] = 'Select a database by name, or none for `default`, through the same endpoint and session; return results.'
elements['authorization']['description'] = 'Derive trusted tenant identity. Resolve the selected (or default) database; verify ownership and grants; stamp database_id into the IR.'
elements['placement-router']['plannedNotes'] = ['Tenant-only seam is landed; control-plane database routing (resolve(databaseId), M4), route grants (M1) and remote execution are planned.']

# ---------------------------------------------------------------- M1 + M4: the query flows ------------------
for id in ['flow-query-right-node', 'flow-query-stale']:
    drop_diagram(id)
QUERY_DOC = ('Success scenario, first request for this database on this gateway (grant miss). Authorization resolves the selection (a name, or none for `default`, M4), asks the control plane once for the '
             'immutable ID and grants, then once for a route grant (M1), stores the grant and dispatches locally. Later requests reuse the grant: see the grant-reuse scenario. The agent checks the grant\'s epoch and '
             'generation against its durable local ACTIVE state before classification. Statements carry operation IR as well as SQL/binds. Deadline failure and stale-owner failure are separate named scenarios; no timing or throughput measurement is claimed.')
STE_QUERY_DOC = ('First request for a database on this gateway. Resolve the selected database, or `default` if the request names none. Ask the control plane once for the ID and permissions. Ask the router once for a route grant. '
                 'Store the grant. Send the request to the owner VM with the grant. The agent checks the epoch and generation against its local state. Return the result through the same path. Later requests reuse the grant.')
flow('flow-query-right-node', 'Query: resolve once, obtain a route grant, execute on the owner (M1, M4)', [
    ('dp-request', F, 'Session names a tenant database, or names none and gets `default` (M4)'),
    ('dp-auth', F, 'Authenticate session; derive trusted tenant; resolve the selection'),
    ('dp-grant', F, 'Look up route grant for (gateway, database)'),
    ('dp-grant', R, 'Miss: no unexpired grant for this database'),
    ('dp-auth-sql', F, 'Resolve name → immutable ID under the trusted tenant; verify grants'),
    ('dp-auth-sql', R, 'Return database ID and the principal\'s grant version'),
    ('dp-route', F, 'Request a route grant for this database'),
    ('dp-placement', F, 'Router reads authoritative ACTIVE VM, epoch and generation'),
    ('dp-placement', R, 'Return node B, epoch and generation; client never learns node B'),
    ('dp-route', R, 'Return route grant: owner B, epoch, generation, grant version, TTL 30 s'),
    ('dp-grant', F, 'Store the grant; reuse until TTL, WrongOwner or revocation'),
    ('dp-forward', F, 'Dispatch locally with the grant, stamped database_id and request identity'),
    ('dp-owner', F, 'Forward tenant/database IDs, grant epoch and generation, plan IR, SQL/binds and deadline to node B'),
    ('dp-classify', F, 'Agent checks grant epoch/generation against durable local ACTIVE state and fence; sends trusted IR to the lane'),
    ('dp-execute', F, 'Choose the execution path from the operation IR'),
    ('dp-deadline', F, 'Apply execution deadline policy'),
    ('dp-files', F, 'Execute against this database file'),
    ('dp-files', R, 'Return rows after statement settles'),
    ('dp-execute', R, 'Return completed execution result'),
    ('dp-classify', R, 'Return lane result to agent admission'),
    ('dp-owner', R, 'Return rows to forwarding gateway'),
    ('dp-forward', R, 'Return rows locally to gateway authorization; no router in result path'),
    ('dp-auth', R, 'Return result to query ingress'),
    ('dp-request', R, 'Return rows to the same client session'),
], QUERY_DOC, STE_QUERY_DOC, 'planes-data-components')
flow('flow-query-grant-reuse', 'Query — later request: the route grant is reused, no control-plane call (M1)', [
    ('dp-request', F, 'Same session, same or another request for this database'),
    ('dp-auth', F, 'Authenticate session; resolve the selection locally'),
    ('dp-grant', F, 'Look up route grant for (gateway, database)'),
    ('dp-grant', R, 'Hit: unexpired grant with matching grant version'),
    ('dp-forward', F, 'Dispatch locally with the cached grant; the router is not called'),
    ('dp-owner', F, 'Forward with grant epoch and generation to the owner'),
    ('dp-classify', F, 'Agent checks epoch/generation against durable local ACTIVE state and fence'),
    ('dp-execute', F, 'Classify and execute'),
    ('dp-files', F, 'Execute against this database file'),
    ('dp-files', R, 'Return rows'),
    ('dp-execute', R, 'Return result'),
    ('dp-classify', R, 'Return lane result'),
    ('dp-owner', R, 'Return rows to the gateway'),
    ('dp-forward', R, 'Return rows to authorization'),
    ('dp-auth', R, 'Return result to ingress'),
    ('dp-request', R, 'Return rows to the client'),
], 'Named steady-state scenario (M1). While a grant is valid the control plane is not on the request path at all: no registry call, no routing call. The agent\'s check of the grant\'s epoch and generation against its durable local state is the enforcement point, so a grant that outlives a move is rejected at the owner, not trusted. A grant expires after its TTL; the next request takes the grant-miss path. This is what the lanes proposal\'s cached placement bought, moved to the gateway, without its lease.',
     'A later request for the same database. The gateway finds a valid grant. It does not call the control plane. It sends the request to the owner with the epoch and generation. The agent checks them. If they do not match, the agent rejects the request. After the time limit, the next request obtains a new grant.', 'planes-data-components')
flow('flow-query-stale', 'Query — alternative: stale grant after a move obtains a fresh decision (M1)', [
    ('dp-owner', F, 'Send request with the cached grant\'s old epoch to former owner A'),
    ('dp-owner', R, 'A rejects before executing: WrongOwner / stale epoch (its durable local state disagrees)'),
    ('dp-forward', R, 'Return WrongOwner to gateway authorization'),
    ('dp-grant', F, 'Invalidate the cached grant for this database'),
    ('dp-route', F, 'Request a fresh route grant: a fresh decision, never a reuse of the old epoch'),
    ('dp-placement', F, 'Router reads authoritative ACTIVE VM, epoch and generation'),
    ('dp-placement', R, 'Return new owner B and new epoch after the completed move'),
    ('dp-route', R, 'Return fresh grant bound to the new epoch and generation'),
    ('dp-grant', F, 'Store the fresh grant'),
    ('dp-forward', F, 'Reopen at the new owner with the fresh grant'),
    ('dp-owner', F, 'Retry safely at B with the same request ID; no client change'),
    ('dp-owner', R, 'Return result or the recorded per-database mutation outcome'),
], 'Named stale-route alternative. A reopen after WrongOwner obtains a fresh decision bound to the new epoch (the DP-01 re-review\'s residual nit, closed here); no grant from the old epoch may be reused. Reject before execution. The illustrated refresh finds a completed move; MOVING/SUSPENDED/DROPPING or unavailable metadata instead returns an unavailable error and does not run SQL. A prior ambiguous mutation result requires request-ID deduplication committed in that database; blind replay is forbidden. Request-ID state moves with the database.',
     'The old owner rejects the stale grant before execution. Invalidate the grant. Obtain a fresh decision for the new epoch. Do not reuse the old grant. Retry at the new owner with the same request ID. Do not blindly retry a mutation if its result is unknown.', 'planes-data-components')
for id in ['flow-query-right-node', 'flow-query-grant-reuse', 'flow-query-stale']:
    document(id)['termIds'] = ['term-route-grant']
    document(id)['sourceIds'] = list(dict.fromkeys(document(id)['sourceIds'] + FABLE + ['src-rereview']))
extend(['flow-query-right-node', 'flow-query-grant-reuse', 'flow-query-stale', 'placement-router'], GRANT, STE_GRANT)
document('placement-router')['termIds'] = list(dict.fromkeys(document('placement-router')['termIds'] + ['term-route-grant']))


# ---------------------------------------------------------------- M3: the lane view inside the agent ----------
LANE_SOURCES = FABLE + ['src-lanes', 'src-lanes-inventory']
LANES_NOTE = ('Lane view (M3): the lanes proposal\'s component view of a statement lane, re-keyed by database_id and placed behind the VM agent. Two additions the planes documents did not state: '
              'backups and backup snapshots run on a read-only connection, never on the writer\'s queue (today Storage.snapshot runs on the writer queue, platform/backup.ts), and placement may pin a database to a dedicated lane. '
              'Lane assignment is tenant-affine by default: a tenant\'s databases share lanes with each other before they share with another tenant\'s, so a kill\'s blast radius is first the tenant\'s own databases. '
              'A read-only snapshot connection costs 2 descriptors per database during the snapshot; a pin is one more placement attribute to operate. Observability (lane JSONL, logger) stays omitted as in DP-01 DP10.')
STE_LANES_NOTE = ('Lane view: this is the lanes proposal\'s view of one lane, keyed by database and placed behind the VM agent. Take backups on a read-only connection. Do not take them on the writer queue. '
                  'Placement can pin a database to its own lane. By default, a tenant\'s databases share a lane with each other before they share with another tenant. A snapshot connection uses 2 descriptors. Logging is not drawn, as in the planes proposal.')
felement('lane-ipc', 'component', 'Lane IPC endpoint',
         'Receives admitted statements from the agent: trusted operation IR, parameterized SQL/binds, database ID, grant epoch, deadline, request ID; returns results, cancels and health.',
         'Receive statements from the agent. Each carries the operation IR, SQL and binds, database ID, deadline and request ID. Return results and health.',
         parent='lanes', tech='JSON-lines over stdio', tags=external, sources=LANE_SOURCES, origin='M3 import of lanes `lane-ipc`, re-keyed by database.')
felement('lane-scheduler', 'component', 'Write-first scheduler + bounded statement pool',
         'Semaphore(1) per database file: a mutation waits only on its own file\'s writer lock, never on read permits. Bounded permit pool for nonblocking steps with a per-database sub-cap; an over-cap database gets a typed QueueFull for itself only.',
         'Use one writer gate for each database file. A write waits only for its own file. Reads use a bounded pool of permits. Each database has its own limit. Reject work over the limit with a QueueFull error for that database only.',
         parent='lanes', status='mixed', tech='Effect Semaphore(1) per file (landed); permit pool (proposed)', tags=external, sources=LANE_SOURCES + ['src-storage'],
         notes=['Semaphore(1) per file and WRITER_QUEUE_CAP are landed in platform/sqlite.ts; the per-database sub-cap, the write-first rule inside a lane process, tenant-affine assignment, pinning and typed QueueFull are proposed.'],
         origin='M3 import of lanes `lane-scheduler` + `lane-pool`, re-keyed by database; mixed because the per-file writer gate is landed.')
felement('lane-handles', 'component', 'Handle LRU / read pool',
         'One writer handle per database and pooled read-only handles, leased, idle-evicted after transactions settle; accounted at 3 descriptors per writer and 2 per read handle against the VM\'s nofile capacity.',
         'Keep one writer handle for each database and a pool of read handles. Lease them. Close idle handles after transactions finish. Count 3 descriptors for a writer and 2 for a read handle.',
         parent='lanes', status='mixed', tech='Leased handle cache (landed 64-entry, 60 s idle); per-database read pool (proposed)', tags=external, sources=LANE_SOURCES + ['src-storage'],
         notes=['The 64-entry leased handle cache with 60 s idle expiry is landed in platform/sqlite.ts; pooled read-only handles per database and descriptor accounting are proposed.'],
         origin='M3 import of lanes `lane-handles`, re-keyed by database.')
felement('lane-request-ids', 'component', 'Request-ID store',
         'Request ID and outcome written inside the mutation\'s own transaction in the database file; looked up before any retry; travels with the database on move and restore. Blind replay is forbidden.',
         'Write the request ID and result in the same transaction as the change. Look it up before a retry. The table is in the database file, so it moves with the database.',
         parent='lanes', tech='SQLite table in the database file', tags=external, sources=LANE_SOURCES, origin='M3 import of lanes `lane-request-ids`, keyed by database.')
felement('lane-snapshot', 'component', 'Read-only snapshot connection',
         'Backups snapshot through a separate read-only connection (VACUUM INTO), so a backup never holds the writer lock; today Storage.snapshot runs on the writer\'s queue. quick_check and SHA-256 run on the copy (M6).',
         'Take backup snapshots on a separate read-only connection. A backup must not hold the writer lock. Today the snapshot runs on the writer queue. Check the copy and compute its checksum.',
         parent='lanes', tech='Read-only SQLite connection; VACUUM INTO', tags=external, sources=FABLE + ['src-backup', 'src-storage', 'src-lanes'],
         origin='M3 snapshots off the writer lock (lanes F4c); feeds M6.')
frel('ln-admit', 'node-admission', 'lane-ipc', 'Dispatch admitted request to the database\'s lane: tenant-affine by default, pinned if placement says so', 'JSON-lines IPC', sources=LANE_SOURCES)
frel('ln-classify', 'lane-ipc', 'classifier', 'Classify trusted operation IR; SQL text is never read', 'In-process call', sources=LANE_SOURCES + ['src-classifier'])
frel('ln-schedule', 'classifier', 'lane-scheduler', 'Bounded plan: writer gate or blocking step; unbounded read: nonblocking permit', 'In-process call', sources=LANE_SOURCES)
frel('ln-handle', 'lane-scheduler', 'lane-handles', 'Acquire this database\'s writer handle or a pooled read handle', 'In-process lease', sources=LANE_SOURCES)
frel('ln-run', 'lane-handles', 'execution', 'Run the statement on the acquired handle', 'Existing FFI binding; proposed lane dispatch', sources=LANE_SOURCES + ['src-ffi'])
frel('ln-dedup', 'execution', 'lane-request-ids', 'Write request ID and outcome inside the mutation\'s transaction; look up before a retry', 'SQL in the same transaction', sources=LANE_SOURCES)
frel('ln-store', 'lane-request-ids', 'files', 'Request-ID rows live in the database file and travel with it', 'SQLite table in the database file', sources=LANE_SOURCES)
frel('ln-backup', 'node-admission', 'lane-snapshot', 'Take a backup snapshot on a read-only connection, off the writer lock (M3, M6)', 'JSON-lines IPC', sources=FABLE + ['src-backup'])
frel('ln-snapshot', 'lane-snapshot', 'files', 'VACUUM INTO from a read-only connection; quick_check and SHA-256 on the copy', 'Existing Deno FFI / SQLite backup', sources=FABLE + ['src-backup', 'src-ffi'])
lane_view = diagram('planes-lane-components', 'component', 'Inside a statement lane: write-first scheduling, handles, request IDs, snapshots (M3)',
                    ['node-admission', 'lane-ipc', 'classifier', 'lane-scheduler', 'lane-handles', 'execution', 'deadline', 'lane-request-ids', 'lane-snapshot', 'files'],
                    ['ln-admit', 'ln-classify', 'ln-schedule', 'ln-handle', 'ln-run', 'dp-deadline', 'dp-interrupt', 'dp-escalate', 'dp-files', 'ln-dedup', 'ln-store', 'ln-backup', 'ln-snapshot'],
                    LANES_NOTE + ' ' + BUDGETS, STE_LANES_NOTE + ' ' + STE_BUDGETS, scope='lanes', parent='planes-agent-components')
lane_view['groups'] = [dict(id='g-lane', label='One lane process: a tenant-affine set of databases; pinnable (M3)',
                            memberIds=['lane-ipc', 'classifier', 'lane-scheduler', 'lane-handles', 'execution', 'deadline', 'lane-request-ids', 'lane-snapshot'], status='planned')]
lane_view['sourceIds'] = list(dict.fromkeys(lane_view['sourceIds'] + LANE_SOURCES))
document('planes-lane-components')['sourceIds'] = list(dict.fromkeys(document('planes-lane-components')['sourceIds'] + LANE_SOURCES))
fdoc('doc-lane-measurements', 'Proof-of-concept measurements the lane design carries (not guarantees)',
     'The lanes dataset (`doc-measurements`) quotes proof-of-concept measurements from vm-1 on 2026-10-12 (language-switch study §3). They are historical observations on one host about the landed FFI path and a prototype lane, not guarantees and not measurements of this design; no label in this dataset carries them: '
     'point read, FFI blocking step, p50 6.3 to 6.8 µs; point read, FFI nonblocking step, p50 44 to 69 µs (p99 240 to 2,180 µs); interrupt latency on the nonblocking step about 0.4 ms; lane kill, SIGKILL to exit, 3.8 ms, and respawn to first answer 51 ms; '
     'file descriptors 3 per writer handle and 2 per read-only handle. The study\'s own limits: one connection in the FFI probe; the blocking pool\'s size and back-pressure under hundreds of long statements were not measured. '
     'M2\'s gate for the in-node step: getGame p50 through a lane against the landed 4.1 ms baseline; more than about 2 ms added reopens the worker variant for reads only.',
     'These values come from tests on one VM on 2026-10-12. They are not guarantees. They do not measure this design. A point read with the blocking step took approximately 7 µs. A point read with the nonblocking step took approximately 44 to 69 µs. '
     'An interrupt stopped a statement in approximately 0.4 ms. A lane kill and restart took approximately 55 ms. A writer handle uses 3 descriptors and a read handle uses 2. The pool size under heavy load was not measured.',
     LANE_SOURCES)
lane_view['docIds'].append('doc-lane-measurements')
elements['lane-scheduler']['docIds'].append('doc-lane-measurements')
extend(['lanes', 'node-admission', 'metadata'], LANES_NOTE, STE_LANES_NOTE)
replace_text(['lifecycle-policies'], 'Backup takes a consistent per-database SQLite backup with an identity/generation manifest and verification; it does not copy a live main file alone.',
             'Backup takes a consistent per-database SQLite backup on a read-only snapshot connection, off the writer lock (M3), and completes only when its manifest is recorded (M6); it does not copy a live main file alone.',
             'Make and verify a consistent database backup.', 'Make a consistent database backup on a read-only connection. Verify it. Record its manifest.')
elements['lanes']['description'] = 'Private agent-managed execution workers; the lane view (M3) shows their internals. Landed FFI code inside proposed lanes.'
for n in containers['nodes']:
    if n['elementId'] == 'lanes':
        n['detail'] = dict(diagramId='planes-lane-components')


# ---------------------------------------------------------------- M6: backup completion is a recorded manifest ----
MANIFEST = ('Backup completion (M6): the landed pipeline (snapshot, quick_check, SHA-256, immutable PUT, manifest, latest.json; platform/backup.ts) runs inside the agent per database, on the read-only snapshot connection (M3). '
            'The backup operation completes only when the agent\'s acknowledgement carries the manifest (object key, checksum, database ID, generation, format) and the control plane records it in the operation record, keyed by (database ID, generation). '
            'A backup is a restore candidate only if it is recorded there: an uploaded but unverified or unrecorded object is not. This is DP-01 tweak 6\'s completion boundary with no new mechanism, and the input M5 needs. '
            'RPO stays the backup interval and RTO stays OPEN; WAL streaming stays deferred.')
STE_MANIFEST = ('Backup completion: the agent makes the snapshot on the read-only connection, checks it, computes its checksum and uploads it. The agent then sends the manifest to the control plane: object key, checksum, database ID, generation and format. '
                'The control plane records the manifest for the database and generation. The backup is complete only after this record exists. Only a recorded backup can be used for a restore. The recovery point is the backup interval. WAL streaming is deferred.')
felement('backup-registry', 'component', 'Backup manifest registry',
         'Records backup manifests keyed by (database ID, generation): object key, checksum, format, completion state. Only a recorded manifest is a restore candidate.',
         STE_MANIFEST, parent='control', tech='Proposed registry logic; Postgres hand-written SQL', sources=FABLE + ['src-backup', 'src-review'], terms=['term-backup-manifest'],
         origin='M6 recorded manifest keyed by (database, generation); closes DP-01 tweak 6\'s completion boundary.')
document('backup-registry')['plain'] = MANIFEST + '\n\n' + SHIFT
frel('cp-manifest', 'lifecycle', 'backup-registry', 'Record a completed manifest; look up the latest recorded manifest for a database', 'In-process call', sources=FABLE + ['src-backup'])
frel('cp-manifests-sql', 'backup-registry', 'metadata', 'Manifest rows: database ID, generation, object key, checksum, format, completion state', 'Postgres hand-written SQL', sources=FABLE + ['src-backup', 'src-decisions'])


def mixed_flow(id, title, entries, plain, ste, parent):
    # Fable flows mix container-tier (c-) and component-tier (cp-) relationships; a container edge shows as 100 + its
    # container number so per-diagram display numbers stay unique while remaining model references.
    d = flow(id, title, entries, plain, ste, parent)
    for e in d['edges']:
        if e['relationshipId'].startswith('c-'):
            e['displayNumber'] = 100 + edge_numbers[e['relationshipId']]
    return d


mixed_flow('flow-backup-manifest', 'Backup: snapshot off the writer lock, upload, verify, record the manifest (M6)', [
    ('cp-lifecycle', F, 'Request backup of one database with operation ID'),
    ('cp-operation', F, 'Persist BACKUP operation bound to database ID and current generation'),
    ('cp-node', F, 'Instruct hosting VM agent (process): snapshot on a read-only connection (M3), quick_check, SHA-256'),
    ('c-node-backup', F, 'Immutable PUT keyed by database ID, generation and snapshot ID'),
    ('c-node-backup', R, 'Store acknowledges the object'),
    ('c-node-backup', F, 'Verify: read back the object\'s checksum and length and compare'),
    ('c-node-backup', R, 'Checksum matches'),
    ('cp-node', R, 'Acknowledge with the manifest: object key, checksum, database ID, generation, format'),
    ('cp-manifest', F, 'Record the manifest; completion state RECORDED'),
    ('cp-manifests-sql', F, 'Manifest row committed with the operation record'),
    ('cp-operation', F, 'Operation COMPLETE only after the manifest is recorded'),
    ('cp-lifecycle', R, 'Report backup recorded: eligible for restore'),
], 'Success scenario (M6). ' + MANIFEST, STE_MANIFEST, 'planes-control-components')
mixed_flow('flow-backup-unverified', 'Backup — alternative: verification fails, nothing is recorded (M6)', [
    ('cp-node', F, 'Instruct snapshot and upload'),
    ('c-node-backup', F, 'PUT object'),
    ('c-node-backup', R, 'Read-back checksum mismatch, or store error'),
    ('cp-node', R, 'Agent reports failure; no manifest'),
    ('cp-operation', F, 'Record FAILED; no manifest row, so the object is not a restore candidate; schedule orphan-object cleanup'),
    ('cp-lifecycle', R, 'Report failed backup; the previous recorded manifest remains the latest candidate'),
], 'Named failure alternative (M6). An object that did not verify, or whose manifest was never recorded, is never a restore candidate even if it exists in the store. The previous recorded manifest stays the latest candidate; orphan objects are cleaned by a later generation-scoped operation.',
     'If the upload does not verify, do not record a manifest. The object is not a restore candidate. The previous recorded backup remains the latest candidate. Remove orphan objects later.', 'planes-control-components')
for id in ['flow-backup-manifest', 'flow-backup-unverified', 'backups', 'lifecycle-policies']:
    document(id)['termIds'] = list(dict.fromkeys(document(id)['termIds'] + ['term-backup-manifest']))
extend(['backups', 'lifecycle', 'lane-snapshot'], MANIFEST, STE_MANIFEST)
elements['backups']['description'] = 'Verified snapshots per database ID and generation; a backup counts only once its manifest is recorded (M6). Optional WAL frames are future work.'

# ---------------------------------------------------------------- M5: VM loss, end to end ------------------------
RECOVERY = ('VM loss (M5, closes DP-01 tweak 6\'s deferral): stale health reports beyond the freshness threshold mark a VM suspect and every database it hosts unavailable; detection is never proof of fencing. '
            'A lifecycle operation recover-from-loss(vm) has a hard precondition: a platform operator attests the VM is isolated, backed by a provider-level proof (the VM is powered off or its network is detached through the provider API), and the control plane records that response. '
            'Then, per database the VM hosted: CAS the placement to FENCED-BY-OPERATOR at a fresh epoch, run the existing restore flow from the latest recorded backup manifest (M6) onto an eligible VM as a fenced generation swap, and activate. '
            'Recovery point is the backup interval, stated as such. Without the attestation the operation is refused and the databases stay unavailable: inability to contact the old writer is never permission to activate a copy. This is explicitly not automatic failover. '
            'For a drop whose hosting agent is permanently lost, the control plane\'s lifecycle identity deletes the backup-store objects itself (the store is not VM-local, so the agent-only rule does not apply to it), and the local files are recorded as destroyed with the attested VM. '
            'If the old VM ever returns, its files are garbage at a dead epoch and the generation-scoped cleanup rule removes them.')
STE_RECOVERY = ('VM loss: if a VM stops reporting, mark it suspect. All its databases become unavailable. Detection does not prove the old writer stopped. '
                'A platform operator must attest that the VM is isolated. The control plane checks this with the provider API: the VM is powered off, or its network is detached. The control plane records the answer. '
                'Then, for each database on that VM: fence the placement at a new epoch, restore the latest recorded backup onto an eligible VM, verify it, and activate it. The recovery point is the backup interval. '
                'Without the attestation, refuse the operation. The databases stay unavailable. Failure to contact the old writer is never permission to activate a copy. This is not automatic failover. '
                'If the hosting agent of a dropped database is lost, the lifecycle identity deletes the backup objects itself. Record the local files as destroyed with the VM.')
felement('platform-operator', 'person', 'Platform operator (human)',
         'Operates the fleet; attests that a lost VM is isolated before any recovery activates a copy elsewhere. Distinct from the customer administrator.',
         'Operates the platform. Attests that a lost VM is isolated before recovery. This is not the customer administrator.', sources=FABLE + ['src-review'], terms=['term-attestation'],
         origin='M5: the human in the loop for every VM loss.')
felement('provider', 'softwareSystem', 'Cloud provider API',
         'Answers whether a VM is powered off or network-detached; its recorded response is the proof behind an isolation attestation. A provider dependency this revision accepts.',
         'The provider API reports if a VM is powered off or disconnected. The control plane records the answer as proof of isolation.', tags=external, sources=FABLE, terms=['term-attestation'],
         origin='M5: provider-level isolation proof (Astra\'s "explicit isolation/proof-of-fence prerequisite" made concrete).')
frel('ctx-attest', 'platform-operator', 'platform', 'Attested recover-from-loss(vm) and fleet oversight', 'HTTPS JSON')
frel('ctx-provider', 'platform', 'provider', 'Confirm a lost VM is powered off or network-detached; record the response', 'Provider HTTPS API')
frel('c-attest', 'platform-operator', 'control', 'recover-from-loss(vm) with isolation attestation', 'HTTPS JSON')
frel('c-provider', 'control', 'provider', 'Verify isolation attestation; record the proof', 'Provider HTTPS API')
frel('cp-attest', 'platform-operator', 'management', 'recover-from-loss(vm) with isolation attestation; per-database outcome', 'HTTPS JSON')
frel('cp-recover', 'management', 'lifecycle', 'Dispatch VM-loss recovery; precondition: recorded attestation', 'Durable operation dispatch')
frel('cp-suspect', 'provisioner', 'lifecycle', 'Health reports stale beyond threshold: mark VM suspect (detection, not fencing proof)', 'In-process signal')
frel('cp-provider', 'lifecycle', 'provider', 'Verify attestation through the provider API; record the response as fence proof', 'Provider HTTPS API')
frel('cp-backup-delete', 'lifecycle', 'backups', 'Delete backup objects of a database whose hosting agent is attested lost', 'Object-store HTTPS (lifecycle identity)')
context['nodes'] += [dict(elementId='platform-operator'), dict(elementId='provider')]
context['edges'] += [dict(relationshipId=r, displayNumber=edge_numbers[r]) for r in ['ctx-attest', 'ctx-provider']]
containers['nodes'] += [dict(elementId='platform-operator'), dict(elementId='provider')]
containers['edges'] += [dict(relationshipId=r, displayNumber=edge_numbers[r]) for r in ['c-attest', 'c-provider']]
cp['nodes'] += [dict(elementId=e) for e in ['platform-operator', 'provider', 'backup-registry', 'backups']]
cp['edges'] += [dict(relationshipId=r, displayNumber=edge_numbers[r]) for r in ['cp-attest', 'cp-recover', 'cp-suspect', 'cp-provider', 'cp-backup-delete', 'cp-manifest', 'cp-manifests-sql']]
cp['edges'].append(dict(relationshipId='c-node-backup', displayNumber=100 + edge_numbers['c-node-backup']))
cp['title'] = 'Control plane: routing authority, placement, lifecycle, backups and recovery (M5, M6, M8)'
mixed_flow('flow-recover-from-loss', 'Recover from VM loss: attested isolation, then restore elsewhere (M5)', [
    ('cp-suspect', F, 'Reports from VM A stale beyond threshold: mark A suspect; every database it hosts is unavailable (detection is not proof of fencing)'),
    ('cp-operation', F, 'Record the VM-loss incident; placements keep their epoch; no copy activates'),
    ('cp-attest', F, 'Platform operator requests recover-from-loss(A) and attests A is isolated'),
    ('cp-recover', F, 'Dispatch recovery with the attestation'),
    ('cp-provider', F, 'Verify: query the provider for A\'s power / network state'),
    ('cp-provider', R, 'Provider confirms A powered off or detached; response recorded as the fence proof'),
    ('cp-operation', F, 'Per hosted database: CAS placement → FENCED-BY-OPERATOR at a fresh epoch, citing the proof'),
    ('cp-manifest', F, 'Look up the latest recorded backup manifest per database (M6)'),
    ('cp-manifest', R, 'Manifest: object key, checksum, database ID, generation, format; unrecorded backups are not candidates'),
    ('cp-operation', F, 'Choose an eligible target VM B (eligibility inputs unchanged; candidate v1 rule pending decision, M8); reserve'),
    ('cp-node', F, 'Instruct target VM agent (process) B: fetch and verify the recorded backup as a new generation'),
    ('c-node-backup', F, 'B fetches the snapshot object'),
    ('c-node-backup', R, 'Object bytes; B verifies checksum against the manifest and runs quick_check'),
    ('cp-node', R, 'B confirms a verified staged generation'),
    ('cp-operation', F, 'CAS FENCED-BY-OPERATOR → ACTIVE on B at the fresh epoch and new generation'),
    ('cp-node', F, 'Activate B only after it reads the committed placement'),
    ('cp-node', R, 'B accepts current-epoch access; stale grants get WrongOwner at B and refresh (M1)'),
    ('cp-operation', F, 'Record recovery complete: recovery point = recorded backup; A\'s files are garbage at a dead epoch, removed by generation-scoped cleanup if A returns'),
    ('cp-recover', R, 'Report recovered databases and recovery point'),
    ('cp-attest', R, 'Operator sees the per-database outcome'),
], 'Success scenario (M5). ' + RECOVERY, STE_RECOVERY, 'planes-control-components')
mixed_flow('flow-recover-refused', 'Recover from VM loss — alternative: no attestation, recovery refused (M5)', [
    ('cp-suspect', F, 'A unreachable; its databases unavailable'),
    ('cp-attest', F, 'Request recover-from-loss(A) without a usable attestation'),
    ('cp-recover', F, 'Dispatch; check the precondition'),
    ('cp-provider', F, 'Query the provider for A\'s state'),
    ('cp-provider', R, 'A reports running, or the provider cannot answer: no isolation proof'),
    ('cp-operation', F, 'Record REFUSED; placements unchanged; no copy activates; databases stay unavailable'),
    ('cp-recover', R, 'Refused: inability to contact the old writer is never permission to activate a copy'),
    ('cp-attest', R, 'Operator is told which proof is missing'),
], 'Named refusal alternative (M5). The standing rule: inability to contact the old writer is never permission to activate a copy without an operator attestation backed by provider proof. The databases stay unavailable; this is the availability price of closing the split-brain window by construction, accepted knowingly.',
     'If there is no attestation, refuse. Keep the databases unavailable. Do not activate a copy because the old VM does not answer. Tell the operator which proof is missing.', 'planes-control-components')
mixed_flow('flow-drop-lost-agent', 'Drop database — alternative: hosting agent permanently lost (M5)', [
    ('cp-node', F, 'Attempt idempotent removal on A after fencing'),
    ('cp-node', R, 'A unreachable; cleanup not confirmed; stay DROPPING'),
    ('cp-operation', F, 'Precondition: A is recorded FENCED-BY-OPERATOR (attested lost); otherwise remain pending as in the unconfirmed-removal alternative'),
    ('cp-backup-delete', F, 'Lifecycle identity deletes this database\'s backup objects itself: the store is not VM-local'),
    ('cp-backup-delete', R, 'Store confirms deletion of every object the manifests name'),
    ('cp-operation', F, 'Record local files as destroyed with the attested VM; remove live rows; keep the non-routable tombstone'),
    ('cp-lifecycle', R, 'Report deletion complete'),
], 'Named alternative (M5) closing DP-01\'s open question of who deletes a lost agent\'s backups. The agent-only rule covers VM-local files; the backup store is not VM-local, so the lifecycle identity may delete there, but only once the VM is attested lost. Without that attestation the drop stays DROPPING with its name reserved, exactly as in the unconfirmed-removal alternative.',
     'If the agent is lost, the lifecycle identity deletes the backup objects. Do this only after the VM is attested lost. Record the local files as destroyed. Without attestation, keep the deletion pending.', 'planes-control-components')
for id in ['flow-recover-from-loss', 'flow-recover-refused', 'flow-drop-lost-agent', 'lifecycle', 'platform-operator', 'provider']:
    document(id)['termIds'] = list(dict.fromkeys(document(id)['termIds'] + ['term-attestation']))
extend(['lifecycle', 'lifecycle-policies', 'flow-drop-failed', 'node', 'planes-control-components'], RECOVERY, STE_RECOVERY)
elements['lifecycle']['description'] = 'Move, suspend/resume, backup/restore, delete and attested recover-from-loss (M5). Fence and drain before changing ownership.'

# ---------------------------------------------------------------- M8: candidate v1 placement rule -------------
POLICY = ('Candidate placement policy v1 (M8) — PROPOSED, PENDING NICK\'S DECISION. This is a decision request, not a settled fact: the DP-01 addendum forbids inventing a solved placement mechanism, and the policy stays OPEN in the registry until Nick decides. '
          'Fable\'s recommendation: among eligible VMs (the DP-01 eligibility inputs unchanged: fresh health, compatible versions, disk with WAL/snapshot/move headroom, resource ceilings, locality), choose the one with the lowest descriptor-weighted load, '
          'where a VM\'s capacity is nofile minus a reserve and each database costs 3 descriptors for its writer plus 2 per pooled reader, with a size-based tiebreak. Weights and locality stay open. '
          'It buys a create flow that runs end to end without a human choosing a VM; it is naive about CPU and I/O and is meant to be replaced by cp-health data once there is some. Origin: lanes tweak 5, the descriptor formula.')
STE_POLICY = ('Candidate placement rule, version 1. This is a proposal. Nick has not decided. Among eligible VMs, choose the VM with the lowest descriptor load. VM capacity is the nofile limit minus a reserve. '
              'Each database costs 3 descriptors for its writer and 2 for each pooled reader. If two VMs are equal, choose by size. Weights and locality remain open. This rule does not consider CPU or I/O. Replace it with health data later.')
felement('placement-policy', 'component', 'Candidate placement policy v1 (proposed, pending decision)',
         'Among eligible VMs pick the lowest descriptor-weighted load: capacity nofile − reserve; 3 descriptors per writer, 2 per pooled reader; size tiebreak. PENDING NICK\'S DECISION.',
         STE_POLICY, parent='control', tech='Proposed ranking over eligible VMs', sources=FABLE + ['src-addendum', 'src-lanes'],
         origin='M8 candidate v1 rule, drawn as a decision request to Nick; the registry keeps placement policy OPEN until he decides.')
document('placement-policy')['plain'] = POLICY + '\n\n' + SHIFT
frel('cp-policy', 'provisioner', 'placement-policy', 'Rank eligible VMs by descriptor-weighted load (candidate v1; pending Nick\'s decision)', 'In-process call', sources=FABLE + ['src-addendum'])
cp['nodes'].append(dict(elementId='placement-policy'))
cp['edges'].append(dict(relationshipId='cp-policy', displayNumber=edge_numbers['cp-policy']))
create = next(d for d in b['diagrams'] if d['id'] == 'flow-create-database')
steps = create['scenario']['children']
i = next(k for k, s in enumerate(steps) if s['label'].startswith('Control plane selects VM'))
steps[i]['label'] = 'Control plane selects VM among eligible VMs (policy OPEN in the registry); reserve non-routable intent'
steps[i + 1:i + 1] = [dict(kind='interaction', id='flow-create-database-s-policy', relationshipId='cp-policy', direction=F, label='Candidate v1 rule ranks eligible VMs (M8; pending Nick\'s decision)'),
                      dict(kind='interaction', id='flow-create-database-s-policy-r', relationshipId='cp-policy', direction=R, label='Lowest descriptor-weighted load, size tiebreak')]
create['edges'].append(dict(relationshipId='cp-policy', displayNumber=edge_numbers['cp-policy']))
create['nodes'].append(dict(elementId='placement-policy'))
extend(['provisioner', 'flow-create-database', 'planes-control-components'], POLICY, STE_POLICY)
elements['provisioner']['description'] = 'Choose VM (placement policy OPEN; candidate v1 rule pending Nick\'s decision, M8). Instruct agent to provision; register placement after confirmation.'


# ---------------------------------------------------------------- M7: instance-level move diagrams -------------
INSTANCES = ('Instance companion (M7, closes DP-01 tweak 7\'s deferral): the participants are instances from the illustrative deployment, so the fence acknowledgement, the agent-to-agent stream and the activation are drawn between distinct boxes: '
             'the management host\'s controller, the metadata Postgres, source VM agent A and target VM agent B. The device is the lanes dataset\'s node-to-node forward (P16): the A → B stream is an instance relationship with no container-level self-edge. c4-sdk draws deployment instances only in deployment views, so these companions are illustrative deployment views whose documents carry the ordered sequence; the step tree stays on the logical move flows. '
             'Ordering is the DP-01 repair\'s: settle requests, checkpoint through a controlled maintenance connection, close that connection and every remaining handle, acknowledge quiescence durably, copy, verify, flip, activate, clean. '
             'Snapshot bytes stream directly from A to B under a move-scoped credential (operation, source, target, database, generation, expiry); the controller carries instructions only. The example moves Alpha/archive (db-a2) from A to B.')
STE_INSTANCES = ('This diagram shows the move between separate boxes: the controller, the metadata database, source agent A and target agent B. The A to B stream is an instance relationship, as in the lanes dataset. '
                 'Order: settle requests; checkpoint with a maintenance connection; close it and all other handles; acknowledge durably; copy; verify; change the owner; activate; clean. '
                 'Snapshot bytes go directly from A to B with a credential for this move only. The controller sends only instructions. The example moves Alpha/archive from A to B.')
frel('dep-a-agent-to-dep-b-agent', 'dep-a-agent', 'dep-b-agent', 'Agent-to-agent snapshot stream under a move-scoped credential (operation, source, target, database, generation, expiry)',
     'Authenticated direct agent-to-agent transfer (proposed)', sources=FABLE + ['src-lanes-inventory', 'src-rereview'],
     origin='M7: instance edge between source and target agents, lanes P16 device; no container self-edge is drawn.')
deployment['edges'].append(dict(relationshipId='dep-a-agent-to-dep-b-agent', displayNumber=edge_numbers['dep-a-agent-to-dep-b-agent']))
def instance_view(id, title, entries, plain, ste):
    # c4-sdk permits deployment instances only in deployment views, so the M7 companions are illustrative deployment
    # views over the move's participants; the ordered sequence lives in the document and in each edge's label list.
    edges = list(dict.fromkeys(r for r, _, _ in entries))
    instances = list(dict.fromkeys(e for r in edges for e in [relationships[r]['sourceId'], relationships[r]['targetId']]))
    hosts = list(dict.fromkeys(elements[i]['parentId'] for i in instances))
    steps = '\n'.join(f'{n}. {"→" if direction == F else "←"} `{r}`: {label}' for n, (r, direction, label) in enumerate(entries, 1))
    d = diagram(id, 'deployment', title, hosts + instances, edges, plain + '\n\nOrdered sequence (→ instruction or transfer, ← acknowledgement):\n\n' + steps,
                ste + '\n\nSequence:\n\n' + steps, parent='planes-deployment', environment='illustrative')
    d['layout']['direction'] = 'LR'
    d['sourceIds'] = list(dict.fromkeys(d['sourceIds'] + FABLE + ['src-rereview']))
    document(id)['sourceIds'] = list(dict.fromkeys(document(id)['sourceIds'] + FABLE + ['src-rereview']))
    d['steps'] = entries  # consumed by the inventory, stripped before writing
    return d


instance_view('flow-move-instances', 'Move Alpha/archive from VM A to VM B: instance participants (M7)', [
    ('dep-control-to-dep-metadata', F, 'CAS ACTIVE → MOVING for db-a2 (Alpha/archive): source A, target B, next epoch; no new grants'),
    ('dep-control-to-dep-a-agent', F, 'Fence db-a2 on A: stop all new admissions, including requests carrying valid grants'),
    ('dep-a-agent-to-dep-alpha-archive', F, 'Settle in-flight requests on the file'),
    ('dep-a-agent-to-dep-alpha-archive', F, 'Checkpoint through a controlled maintenance connection'),
    ('dep-a-agent-to-dep-alpha-archive', F, 'Close the maintenance connection and every remaining handle; confirm worker exit if a kill was needed'),
    ('dep-control-to-dep-a-agent', R, 'A acknowledges quiescence durably: fence recorded, no live handles or writers'),
    ('dep-control-to-dep-b-agent', F, 'Prepare B: reserve staging space for generation g+1; issue the move-scoped transfer credential'),
    ('dep-control-to-dep-a-agent', F, 'Instruct A to stream the closed, checkpointed file and request-ID state to B under that credential'),
    ('dep-a-agent-to-dep-b-agent', F, 'Snapshot bytes stream directly A → B; the controller carries instructions only'),
    ('dep-a-agent-to-dep-b-agent', R, 'B verifies checksum and quick_check; acknowledges to A'),
    ('dep-control-to-dep-b-agent', R, 'B reports a verified staged generation; A stays fenced'),
    ('dep-control-to-dep-metadata', F, 'Transaction: flip owner A → B, increment epoch, publish ACTIVE'),
    ('dep-control-to-dep-b-agent', F, 'Activate B only after it reads the committed owner / epoch'),
    ('dep-control-to-dep-b-agent', R, 'B accepts current-epoch access; stale grants get WrongOwner and refresh (M1)'),
    ('dep-a-agent-to-dep-alpha-archive', F, 'Remove the fenced source copy and sidecars on A, naming the exact old generation and operation'),
    ('dep-control-to-dep-metadata', F, 'Record move complete; keep epoch and operation history'),
], 'Instance-level success companion (M7) of the logical move flow. ' + INSTANCES, STE_INSTANCES)
instance_view('flow-move-failed-instances', 'Move — alternative (instances): B\'s verification fails before the flip (M7)', [
    ('dep-control-to-dep-a-agent', R, 'A fenced and quiescent'),
    ('dep-a-agent-to-dep-b-agent', F, 'Stream the snapshot to B'),
    ('dep-a-agent-to-dep-b-agent', R, 'B: integrity check fails'),
    ('dep-control-to-dep-b-agent', R, 'B reports failed verification'),
    ('dep-control-to-dep-metadata', F, 'Keep MOVING, owner unchanged; record the failed attempt'),
    ('dep-control-to-dep-b-agent', F, 'Remove the staged copy on B; require confirmation'),
    ('dep-control-to-dep-b-agent', R, 'B confirms it cannot serve and the staged copy is gone'),
    ('dep-control-to-dep-metadata', F, 'CAS rollback: A ACTIVE at a fresh epoch; retain failure history'),
    ('dep-control-to-dep-a-agent', F, 'Unfence A only after it reads the rollback epoch'),
    ('dep-control-to-dep-a-agent', R, 'A serves refreshed grants'),
], 'Instance-level pre-flip failure companion (M7) of the logical failure flow. The rollback needs the source intact and verified and confirmed target cleanup, and uses a fresh epoch; after a placement commit no rollback exists and the controller reconciles forward. ' + INSTANCES, STE_INSTANCES)
CLOSED7 = ('DP-01 tweak 7 companion APPLIED in this revision (M7): see flow-move-instances and flow-move-failed-instances, where source agent A, target agent B, the controller and the metadata store are distinct instance participants. '
           'The logical node participant here still denotes the VM agent type. Ordering correction retained: settle requests, checkpoint through a controlled maintenance connection, close that connection and all remaining handles, then durably acknowledge quiescence and copy.')
STE_CLOSED7 = ('The instance companion diagrams now exist. They show source agent A, target agent B, the controller and the metadata store as separate boxes. The order is: settle requests; checkpoint with a maintenance connection; close it and all other handles; acknowledge durably; copy.')
replace_text(['flow-move-database', 'flow-move-failed'], DEFER7, CLOSED7, STE_DEFER7, STE_CLOSED7)
CLOSED6 = ('DP-01 tweak 6 deferral CLOSED in this revision: backup completion is a recorded manifest (M6, flow-backup-manifest); VM loss is detection, operator attestation with provider proof, fenced restore-elsewhere and activation (M5, flow-recover-from-loss); '
           'a lost agent\'s backups are deleted by the lifecycle identity once the VM is attested lost (M5, flow-drop-lost-agent). What stays OPEN: RPO is the backup interval, RTO is unstated, WAL streaming is deferred. '
           'M1 and M5 alter contracts Astra reviewed and passed, so they need Nick\'s word and a re-review, not just this dataset.')
STE_CLOSED6 = ('The backup, VM-loss and lost-agent flows now exist in this revision. The recovery point is the backup interval. The recovery time is not stated. WAL streaming is deferred. M1 and M5 change reviewed contracts, so they need Nick\'s approval and a new review.')
replace_text(['backups', 'lifecycle-policies', 'planes-control-components', 'flow-drop-database', 'flow-drop-failed'], DEFER6, CLOSED6, STE_DEFER6, STE_CLOSED6)

# ---------------------------------------------------------------- M2: build order (documents only) ---------------
BUILD_ORDER = ('Build order (M2, a sequencing decision, not architecture): (1) the in-node half first on one VM: agent plus lanes plus bounded pools, with handles, queues and request IDs keyed by database_id; '
               '(2) the metadata schema and a default database per tenant (M4); (3) gateway, route grants (M1), forwarder; (4) lifecycle flows, backups (M6), recovery (M5). '
               'Step 1 is accepted by the lanes proposal\'s two acceptance tests re-targeted to databases (one database runs a 2 s statement while another\'s once-a-minute mutation commits within 50 ms; a 1 GB backup runs while that file keeps committing), '
               'and gated by the lanes measurement: getGame p50 through a lane against the landed 4.1 ms baseline, with more than about 2 ms added reopening the worker variant for reads only. '
               'This fixes the contention that violates "hot write never starved" before any placement exists and measures the riskiest cost, lane IPC on the hot path, first. It defers the visible product feature (many databases) by one step.')
STE_BUILD_ORDER = ('Build order: first build the agent, lanes and bounded pools on one VM, keyed by database. Second, add the metadata schema and a default database for each tenant. Third, add the gateway, route grants and forwarder. Fourth, add the lifecycle, backup and recovery flows. '
                   'Accept step one with the two lanes tests, applied to databases. Measure the point-read latency through a lane against the 4.1 ms baseline. If a lane adds more than about 2 ms, use the worker variant for reads.')
fdoc('doc-build-order', 'Build order: in-node half first, keyed by database (M2)', BUILD_ORDER, STE_BUILD_ORDER, FABLE + ['src-lanes-inventory'])
for d in [context, lane_view]:
    d['docIds'].append('doc-build-order')


# =====================================================================================================
# Part C — finalise: trailers, source ids, order, navigation, bundle and inventory.
# =====================================================================================================
# Rename the carried-over DP-01 source ids so the DP-02 brief can be `src-brief` in this dataset.
RENAME = {'src-brief': 'src-dp01-brief', 'src-addendum': 'src-dp01-addendum', 'src-review': 'src-dp01-review', 'src-dp02-brief': 'src-brief'}
for s in SOURCES:
    s['id'] = RENAME.get(s['id'], s['id'])
for obj in b['documents'] + b['model']['elements'] + b['model']['relationships'] + b['diagrams']:
    obj['sourceIds'] = list(dict.fromkeys(RENAME.get(s, s) for s in obj['sourceIds']))
    obj['sourceIds'] = list(dict.fromkeys(obj['sourceIds'] + ['src-planes', 'src-comparison']))
# Every explanation carries the (grant-aware) routing and agent notes, and the Fable trailer.
for d in b['documents']:
    for note in [ROUTING, AGENT]:
        if note not in d['plain']:
            d['plain'] += '\n\n' + note
    for note in [STE_ROUTING, STE_AGENT]:
        if note not in d['ste']['text']:
            d['ste']['text'] += '\n\n' + note
    d['plain'] += '\n\n' + FABLE_SHIFT
    d['ste']['text'] += '\n\n' + STE_FABLE_SHIFT
    assert d['ste']['text'].strip() and d['plain'].strip()
for obj in b['diagrams'] + b['model']['elements']:
    document(obj['id'])['title'] = obj.get('title', obj.get('name'))
# Diagram order: static views, then flows under their parents, then the deployment and its instance flows.
ORDER = ['planes-context', 'planes-containers', 'planes-control-components', 'planes-data-components', 'planes-agent-components', 'planes-lane-components',
         'flow-create-database', 'flow-create-failed', 'flow-query-right-node', 'flow-query-grant-reuse', 'flow-query-stale', 'flow-query-deadline',
         'flow-move-database', 'flow-move-failed', 'flow-backup-manifest', 'flow-backup-unverified', 'flow-recover-from-loss', 'flow-recover-refused',
         'flow-drop-database', 'flow-drop-failed', 'flow-drop-lost-agent', 'planes-deployment', 'flow-move-instances', 'flow-move-failed-instances']
assert sorted(ORDER) == sorted(d['id'] for d in b['diagrams']), sorted(d['id'] for d in b['diagrams'])
b['diagrams'].sort(key=lambda d: ORDER.index(d['id']))
# Navigation: dynamic siblings link each other; static views and the deployment link their children and companions.
for d in b['diagrams']:
    if d['kind'] == 'dynamic':
        siblings = [x for x in b['diagrams'] if x['kind'] == 'dynamic' and x['parent'] == d['parent'] and x['id'] != d['id']]
        d['related'] = [dict(diagramId=x['id'], label=x['title']) for x in siblings]
for d in [context, containers, cp, dp, agent_detail, lane_view, deployment]:
    targets = [x for x in b['diagrams'] if x.get('parent', {}).get('diagramId') == d['id']]
    d['related'] = [dict(diagramId=x['id'], label=x['title']) for x in targets]
for logical, companion in [('flow-move-database', 'flow-move-instances'), ('flow-move-failed', 'flow-move-failed-instances')]:
    for a, z in [(logical, companion), (companion, logical)]:
        d = next(x for x in b['diagrams'] if x['id'] == a)
        d['related'].append(dict(diagramId=z, label=next(x for x in b['diagrams'] if x['id'] == z)['title']))
dp['related'].append(dict(diagramId=lane_view['id'], label=lane_view['title']))
for d in b['diagrams']:
    d['legend']['entries'].append(dict(id='instance', label='Instance (deployment / instance flows)', description='A deployment instance of a logical container; the M7 move companions use instances as participants.', swatch=dict(shape='box', tone='muted', stroke='dashed')))

instance_steps = {d['id']: d.pop('steps') for d in b['diagrams'] if 'steps' in d}
CONTENT.mkdir(parents=True, exist_ok=True)
BUNDLE.write_text(json.dumps(b, ensure_ascii=False, indent=2) + '\n')

lines = ['# DP-02 inventory: `mansio-planes-fable`', '',
         'Date: 2026-10-20. Builder: `claude-fable` (Claude Fable 5.1). Proposal only; no runtime code is changed. This dataset is `mansio-planes-proposal` (DP-01, landed `eb5df16`) with modifications M1–M8 from `docs/architecture/scale-out-comparison-fable.md` §4 applied. Generated by `agent-work/items/dp-02-fable-planes/build-dataset.py`, whose Part A is the DP-01 generator (it reproduces the planes bundle byte for byte), so every element, relationship, step and document an M-number does not name is carried over unchanged.', '',
         SHIFT, '', FABLE_SHIFT, '',
         '## Sources', '', '| Source ID | Commit-pinned path | Use |', '| --- | --- | --- |']
for s in SOURCES:
    lines.append(f'| `{s["id"]}` | [{s["path"]}]({s["url"]}) | {s["title"]} |')
lines += ['', '## Fable modifications (the patch)', '',
          'Each F-decision realises one M of the comparison document; "Where" names what changed in the bundle. M1 and M5 alter contracts Astra reviewed and passed in DP-01, so they are proposals needing Nick\'s word and a re-review, not settled design.', '',
          '- **F1 / M1 — route grants with a TTL.** New component `grant-cache` (data plane) and document `doc-grant-cache`; relationships `dp-grant` (authorization ↔ cache) and `dp-revoke` (router → cache, best effort); relabelled `dp-route`, `dp-forward`, `dp-owner`, `c-resolve`, `c-forward`. The DP-01 availability table ("online one-request decision, no reuse") is replaced in `doc-node-admission`, `doc-placement-router`, `doc-flow-move-database` and `doc-flow-move-failed` by the grant table (row "Authority unavailable, running agent" → "existing grants serve until their TTL; no new grants"). `flow-query-right-node` is the grant-miss path, new `flow-query-grant-reuse` is the steady state with no control-plane call, and `flow-query-stale` states that a stale-route retry obtains a **fresh decision** bound to the new epoch (closing the re-review\'s residual nit). The agent\'s check of grant epoch/generation against its durable local ACTIVE state remains the enforcement point; there is no lease and no clock in the fencing argument. Term `term-route-grant`.',
          '- **F2 / M2 — build order.** Document `doc-build-order` (on `planes-context` and `planes-lane-components`): in-node half first on one VM keyed by database_id, then schema + default database, then gateway + grants + forwarder, then lifecycle/backup/recovery; the lanes acceptance tests re-targeted to databases and the 4.1 ms / +2 ms gate. No diagram change: sequencing is not architecture.',
          '- **F3 / M3 — the lane view inside the agent.** New component view `planes-lane-components` (scope `lanes`, child of `planes-agent-components`) importing the lanes proposal\'s lane components re-keyed by database: `lane-ipc`, `lane-scheduler` (mixed: Semaphore(1) per file and WRITER_QUEUE_CAP are landed), `lane-handles` (mixed: the 64-entry leased cache is landed), `lane-request-ids`, `lane-snapshot` (new: backups on a read-only connection, off the writer lock; today `Storage.snapshot` runs on the writer queue), plus the carried `classifier`, `execution`, `deadline`. Relationships `ln-*`. Lane assignment is tenant-affine by default and placement may pin a database to a lane (noted in `doc-node-admission`, `doc-metadata`, `doc-lanes`). `doc-lane-measurements` carries the lanes PoC numbers with provenance and the statement that they do not measure this design. The lanes logger / JSONL are deliberately not imported: observability stays omitted as in DP-01 DP10.',
          '- **F4 / M4 — default database and selection surface.** `doc-ingress`, `doc-authorization`, `doc-placement-router`, `doc-caller`, `doc-database-registry` gain the selection contract: `database` option per session or per call, tenant-scoped name, none → reserved `default`, name → immutable ID resolved under the trusted tenant and stamped into the operation IR after authorization; the classifier and agent never see a name. Migration: each existing tenant file becomes its tenant\'s `default` (minted ID, placement row owner = the one node, epoch 1); `ControlPlaneRouter` becomes `resolve(databaseId)` with `TenantContext` extended. Schema question: Fable\'s default is a per-database schema fingerprint, not yet decided. Term `term-default-database`. Step 1 of the query flows names the default.',
          '- **F5 / M5 — VM loss end to end.** New person `platform-operator` and external system `provider`; relationships `ctx-attest`, `ctx-provider`, `c-attest`, `c-provider`, `cp-attest`, `cp-recover`, `cp-suspect`, `cp-provider`, `cp-backup-delete`. Flows `flow-recover-from-loss` (detection → attestation → provider proof recorded → per database CAS to FENCED-BY-OPERATOR at a fresh epoch → restore from the latest recorded manifest onto an eligible VM → verify → CAS ACTIVE → activate), `flow-recover-refused` (no attestation → refused; databases stay unavailable) and `flow-drop-lost-agent` (lifecycle identity deletes backup objects only once the VM is attested lost). Standing rule in every one of them: inability to contact the old writer is never permission to activate a copy. Recovery point is the backup interval; this is not automatic failover. Term `term-attestation`. DP-01 tweak 6\'s deferral text is replaced by a closure note in `doc-backups`, `doc-lifecycle-policies`, `doc-planes-control-components`, `doc-flow-drop-database`, `doc-flow-drop-failed`.',
          '- **F6 / M6 — backup completion as a recorded manifest.** New component `backup-registry` (control plane), relationships `cp-manifest`, `cp-manifests-sql`; flows `flow-backup-manifest` (snapshot off the writer lock → immutable PUT → read-back verify → acknowledgement carries the manifest → recorded, keyed by (database ID, generation) → eligible for restore) and `flow-backup-unverified` (no manifest, not a candidate). Term `term-backup-manifest`. The landed pipeline in `platform/backup.ts` is the mechanism, re-keyed.',
          '- **F7 / M7 — instance-level move diagrams.** `flow-move-instances` and `flow-move-failed-instances` (children of `planes-deployment`, linked from the logical move flows and back) with instance participants `dep-control`, `dep-metadata`, `dep-a-agent`, `dep-b-agent`. New instance relationship `dep-a-agent-to-dep-b-agent` for the A → B snapshot stream, following the lanes dataset\'s P16 node-to-node forward: an instance edge with no container-level self-relationship and therefore no `modelRelationshipId` (the only deployment-tier edge without one; every other `dep-*` edge still names its logical relationship). c4-sdk permits deployment instances only in deployment views, so the companions are illustrative deployment views (not dynamic step trees) whose documents and this inventory carry the ordered sequence; the step trees stay on the logical flows. The DP-01 repair\'s checkpoint ordering is kept step for step. DP-01 tweak 7\'s deferral text is replaced by a closure note in both logical move documents.',
          '- **F8 / M8 — candidate v1 placement rule, pending Nick\'s decision.** New component `placement-policy` (control plane) and relationship `cp-policy`; two steps inserted into `flow-create-database`. Its documents say **proposed, pending Nick\'s decision** and that the registry keeps placement policy OPEN until he decides; the addendum\'s prohibition on inventing a solved mechanism is respected by labelling, not by omission. Rule: among eligible VMs, lowest descriptor-weighted load (capacity nofile − reserve; 3 descriptors per writer, 2 per pooled reader; size tiebreak); weights and locality open.',
          '- **F9 — sources and pins.** Every carried-over source is re-pinned to `main` at `3d25004` (all paths exist there; nothing under `core/`, `platform/` or `sdk/` changed since `2d30330`). DP-01\'s brief, addendum and review are `src-dp01-*`; `src-brief` is the DP-02 brief at its own commit `b2b65b0`; the DP-01 addendum needs no SHA-256 special case because it is now on `main`. Every element, relationship, diagram and document also cites `src-planes` and `src-comparison`.',
          '- **F10 — mixed-tier edge numbers.** Fable flows that mix container-tier `c-` and component-tier `cp-` relationships (backup and recovery) show a container edge as 100 + its container number so per-diagram display numbers stay unique while remaining model references; `c-node-backup` is also drawn that way in the control-plane view. Carried-over diagrams keep DP-01\'s numbers.',
          '- **F11 — what did not change.** Registries, epoch-checked and per-database serialised lifecycle commands, the trust boundary, bounded scheduling keyed by database with tenant/VM ceilings, the offline move protocol and its fencing rule, drop/tombstone semantics, status marking (`existing` classifier/execution, `mixed` deadline/placement-router, all processes `planned`) and the illustrative deployment are carried over unchanged. The three existing datasets and their content trees are byte-identical to `main`.',
          '', '## Decisions carried over from DP-01 (A1–A4, DP1–DP13)', '',
          'These stand as recorded in `pages/content/mansio-planes-proposal/inventory.md`, with these amendments: A3 gains the F8 candidate rule (still OPEN); DP4 gains the F4 selection surface and migration; DP6 and DP7\'s admission contract is the F1 grant table instead of single-use decisions; DP8 and DP10 lose their tweak 6 deferral (F5, F6); DP7 loses its tweak 7 companion deferral (F7); DP9 gains the lane view, tenant-affine assignment and pinning (F3). The DP-01 repair dispositions for tweaks 1, 3, 4, 5 and 8 are unchanged.',
          '', '## Elements', '', '| ID | Kind / status | Source IDs | Document IDs | Grounding / design |', '| --- | --- | --- | --- | --- |']
for e in b['model']['elements']:
    lines.append(f'| `{e["id"]}` | {e["kind"]} / {e["status"]} | ' + ', '.join('`' + s + '`' for s in e['sourceIds']) + ' | ' + ', '.join('`' + s + '`' for s in e['docIds']) + ' | ' + provenance[e['id']].replace('|', '/') + ' |')
lines += ['', '## Relationships', '', '| ID | Endpoints | Status | Sources / logical instance | Grounding |', '| --- | --- | --- | --- | --- |']
for r in b['model']['relationships']:
    sources = ', '.join('`' + s + '`' for s in r['sourceIds'])
    if 'modelRelationshipId' in r:
        sources += ' / `' + r['modelRelationshipId'] + '`'
    lines.append(f'| `{r["id"]}` | `{r["sourceId"]}` → `{r["targetId"]}` | {r["status"]} | {sources} | {provenance[r["id"]].replace("|", "/")} |')
lines += ['', '## Terms', '', '| ID | Term | Definition |', '| --- | --- | --- |']
for t in b['terms']:
    lines.append(f'| `{t["id"]}` | {t["text"]} | {t["definition"]} |')
lines += ['', '## Views and steps', '', 'Each step inherits its diagram\'s provenance (`src-dp01-brief` / `src-lanes` / `src-dp01-addendum` / `src-dp01-review` for carried-over views; `src-brief` / `src-comparison` / `src-planes` for Fable views, which are marked with their M-number in the title). The authored labels specify the proposed protocol; only the identified classifier/FFI operations are landed.', '']
for d in b['diagrams']:
    lines += [f'### `{d["id"]}` — {d["kind"]}', '', f'Document(s): ' + ', '.join('`' + x + '`' for x in d['docIds']) + f'. Status: {d["status"]}.', '']
    if 'scenario' in d:
        lines += [f'Step root `{d["scenario"]["id"]}`: sequence.', '', '| Step ID | Relationship / direction | Label / grounding |', '| --- | --- | --- |']
        for s in d['scenario']['children']:
            lines.append(f'| `{s["id"]}` | `{s["relationshipId"]}` / {s["direction"]} | {s["label"].replace("|", "/")} |')
        lines += ['']
    elif d['id'] in instance_steps:
        lines += ['Illustrative deployment view (c4-sdk permits instances only there); ordered sequence:', '', '| # | Relationship / direction | Label / grounding |', '| --- | --- | --- |']
        for n, (r, direction, label) in enumerate(instance_steps[d['id']], 1):
            lines.append(f'| {n} | `{r}` / {direction} | {label.replace("|", "/")} |')
        lines += ['']
    else:
        lines += ['Members: ' + ', '.join('`' + n['elementId'] + '`' for n in d['nodes']) + '.', '', 'Edges: ' + ', '.join('`' + e['relationshipId'] + '`' for e in d['edges']) + '.', '']
lines += ['## Learning preservation handoff', '',
          'Durable findings for `docs/knowledge/` (outside this unit\'s Owns set; coordinator to link after review): the route-grant versus lease distinction (F1: no clock in the fencing argument; the agent epoch check is the enforcement point), the attestation precondition for any recovery that activates a copy (F5), and the manifest-as-completion rule (F6). `build-dataset.py` here is a unit-specific authoring source, as DP-01\'s was; its Part A / Part B split (reproduce the base, then patch) is the reusable device for future dataset revisions.', '']
(CONTENT / 'inventory.md').write_text('\n'.join(lines))
print(f'{len(b["model"]["elements"])} elements, {len(b["model"]["relationships"])} relationships, {len(b["diagrams"])} diagrams, {len(b["documents"])} documents, {len(SOURCES)} sources')
