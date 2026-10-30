#!/usr/bin/env python3
"""Trusted local lifecycle observations; never a service attestation endpoint.

hold-container keeps all kernel descriptors in this process across the caller's
existing stop operation. Its anonymous pipe output is not persistent evidence.
"""
import argparse
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import select
import shutil
import stat
import subprocess
import sys
import time

UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\Z")
DIGEST = re.compile(r"[0-9a-f]{64}\Z")
SLOTS = {"media-plane", "media-plane-blue", "media-plane-green"}
COUNTS = {"legacyUsableSIDs", "registryRows", "authConnectionRows", "authRoomBindingRows",
          "dmParentRows", "credentialParentRows", "authParentRows", "dmReconciliationRows",
          "authConnectionClosureRows", "authRoomClosureRows", "retiredBootRows"}
RETAINED = {"authConnectionClosureRows", "authRoomClosureRows", "retiredBootRows"}
STATUS_FIELDS = COUNTS | {"schema", "version", "protocol", "familyActivation", "familyActivationId",
                          "familyRows", "familySIDRows", "oldestUnsettledSeconds",
                          "securityRoundFresh", "productiveGrantEligible"}
NS_GET_PARENT = 0xB702


class Refusal(Exception):
    """A failed observation is never empty-state evidence."""


class OperatorRefusal(Refusal):
    """A refusal whose fixed, identifier-free text the operator must act on."""


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise Refusal("duplicate JSON key")
        value[key] = item
    return value


def decode(raw, limit=65536):
    if len(raw) > limit:
        raise Refusal("observation exceeds bound")
    try:
        return json.loads(raw, object_pairs_hook=unique_object,
                          parse_constant=lambda _: (_ for _ in ()).throw(Refusal("nonfinite JSON")))
    except (ValueError, UnicodeError) as error:
        raise Refusal("invalid JSON observation") from error


def strict_status(raw):
    value = decode(raw)
    if not isinstance(value, dict) or set(value) != STATUS_FIELDS:
        raise Refusal("unknown status schema")
    if value["schema"] != "concord.voice-enforcement-status" or type(value["version"]) is not int or value["version"] != 1 or type(value["protocol"]) is not int or value["protocol"] != 4:
        raise Refusal("unsupported status version")
    if any(type(value[key]) is not int or value[key] < 0 for key in COUNTS | {"familyRows", "familySIDRows"}):
        raise Refusal("invalid status count")
    activation, identity = value["familyActivation"], value["familyActivationId"]
    if activation not in ("prepared", "active") or (activation == "prepared" and identity is not None) or (activation == "active" and (not isinstance(identity, str) or not UUID.fullmatch(identity))):
        raise Refusal("inconsistent activation")
    age = value["oldestUnsettledSeconds"]
    if age is not None and (type(age) is not int or age < 0):
        raise Refusal("invalid pending age")
    if any(type(value[key]) is not bool for key in ("securityRoundFresh", "productiveGrantEligible")):
        raise Refusal("invalid grant observation")
    if activation == "prepared" and value["productiveGrantEligible"]:
        raise Refusal("prepared productive grant")
    return value


def run_json(*args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            check=False, timeout=10)
    if result.returncode:
        raise Refusal("runtime inspection failed")
    return decode(result.stdout, 2 * 1024 * 1024)


def inspect_container(identity):
    rows = run_json("docker", "inspect", "--type", "container", identity)
    if not isinstance(rows, list) or len(rows) != 1 or rows[0].get("Id") != identity:
        raise Refusal("container identity changed")
    return rows[0]


def namespace_identity(fd):
    info = os.fstat(fd)
    return info.st_dev, info.st_ino


def process_start(pid):
    # comm can contain spaces and parentheses. The final ')' precedes state.
    text = Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
    tail = text[text.rindex(")") + 2:].split()
    return tail[19]


def timestamp(value):
    try:
        result = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        if result.tzinfo is None or result.year < 1970:
            raise ValueError()
        return result
    except (ValueError, TypeError, AttributeError) as error:
        raise Refusal("invalid runtime timestamp") from error


def slot_owners(project, slot):
    result = subprocess.run(["docker", "ps", "-a", "--no-trunc", "--filter",
                             f"label=com.docker.compose.project={project}", "--filter",
                             f"label=com.docker.compose.service={slot}", "--format", "{{.ID}}"],
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10, check=False)
    if result.returncode or len(result.stdout) > 65536:
        raise Refusal("slot inventory failed")
    return result.stdout.decode("ascii").splitlines()


PROJECT = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}\Z")
DOCKER_TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z\Z")
ASSOCIATION_FIELDS = {"schema", "version", "composeProject", "slot", "containerId", "imageId",
                      "createdAt", "startedAt", "finishedAt", "restartCount", "nodeBootId"}


def runtime_time(value):
    if not isinstance(value, str) or len(value.encode("utf-8")) > 64 or not DOCKER_TIME.fullmatch(value):
        raise Refusal("invalid Docker timestamp")
    # Retain nanoseconds for ordering: datetime truncates them to microseconds.
    timestamp(value)
    base, _, fraction = value[:-1].partition(".")
    return base, fraction.ljust(9, "0")


def stopped_identity(identity, slot, project, allow_dead=False):
    if not isinstance(project, str) or not PROJECT.fullmatch(project) or slot not in SLOTS | {"control-plane"} or not DIGEST.fullmatch(identity):
        raise Refusal("invalid configured identity")
    value = inspect_container(identity)
    labels = value.get("Config", {}).get("Labels", {})
    if labels.get("com.docker.compose.project") != project or labels.get("com.docker.compose.service") != slot or labels.get("com.docker.compose.oneoff", "False").lower() != "false" or slot_owners(project, slot) != [identity]:
        raise Refusal("ambiguous configured slot")
    state = value["State"]
    if state.get("Status") not in (("exited", "dead") if allow_dead else ("exited",)) or any(state.get(key) is not False for key in ("Running", "Paused", "Restarting")) or type(state.get("Pid")) is not int or state["Pid"] != 0:
        raise Refusal("container is not truthfully stopped")
    image, count = value.get("Image"), value.get("RestartCount")
    if not isinstance(image, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", image) or type(count) is not int or count < 0:
        raise Refusal("invalid runtime identity")
    created, started, finished = value.get("Created"), state.get("StartedAt"), state.get("FinishedAt")
    if not runtime_time(created) <= runtime_time(started) < runtime_time(finished):
        raise Refusal("invalid stopped history")
    return dict(composeProject=project, slot=slot, containerId=identity, imageId=image,
                createdAt=created, startedAt=started, finishedAt=finished, restartCount=count)


def association_value(raw):
    if isinstance(raw, bytes):
        raw = raw.decode("utf-8")
    if len(raw.encode("utf-8")) > 4096:
        raise Refusal("stopped association exceeds byte bound")
    value = decode(raw, 4096)
    if not isinstance(value, dict) or set(value) != ASSOCIATION_FIELDS or value["schema"] != "concord.stopped-media-association" or type(value["version"]) is not int or value["version"] != 1:
        raise Refusal("unknown stopped association schema")
    if not isinstance(value["composeProject"], str) or not PROJECT.fullmatch(value["composeProject"]) or value["slot"] not in SLOTS:
        raise Refusal("invalid stopped slot")
    if not isinstance(value["containerId"], str) or not DIGEST.fullmatch(value["containerId"]) or not isinstance(value["imageId"], str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", value["imageId"]):
        raise Refusal("invalid stopped artifact")
    if not isinstance(value["nodeBootId"], str) or not UUID.fullmatch(value["nodeBootId"]) or type(value["restartCount"]) is not int or value["restartCount"] < 0:
        raise Refusal("invalid stopped boot")
    if not runtime_time(value["createdAt"]) <= runtime_time(value["startedAt"]) < runtime_time(value["finishedAt"]):
        raise Refusal("invalid stopped timestamps")
    return value


def match_association(value, identity, slot, project, allow_dead=False):
    current = stopped_identity(identity, slot, project, allow_dead)
    if any(value[key] != item for key, item in current.items()):
        raise Refusal("stopped association no longer matches")
    return current


def metadata(info):
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


class AssociationStore:
    """Pinned private state under the already-held canonical deployment lock."""
    def __init__(self, lock_path):
        self.fds = []
        lock = Path(lock_path)
        if not lock.is_absolute() or str(lock) != lock_path or ".." in lock.parts:
            raise Refusal("invalid deployment lock")
        managed = lock_path == "/var/lib/concord/deploy-transaction.lock"
        if not managed and lock.name != ".deploy-transaction.lock":
            raise Refusal("unknown deployment lock")
        self.parent = lock.parent
        self.trusted_uid = os.stat(lock_path, follow_symlinks=False).st_uid
        self.name = "voice-restart-associations" if managed else ".voice-restart-associations"
        self.parent_fd = self.open_trusted_directory(self.parent)
        lock_fd = os.open(lock.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, dir_fd=self.parent_fd)
        self.fds.append(lock_fd)
        self.lock_info = os.fstat(lock_fd)
        if not stat.S_ISREG(self.lock_info.st_mode) or (stat.S_IMODE(self.lock_info.st_mode) != 0o600 if managed else self.lock_info.st_mode & 0o022) or self.lock_info.st_nlink != 1 or self.lock_info.st_uid not in (0, os.geteuid()) and os.geteuid() != 0:
            raise Refusal("untrusted deployment lock")
        self.uid, self.gid = self.lock_info.st_uid, self.lock_info.st_gid
        if self.uid != self.trusted_uid:
            raise Refusal("deployment lock ownership changed")
        if os.geteuid() not in (0, self.uid):
            raise Refusal("caller does not own deployment state")
        if not managed:
            try:
                os.mkdir(self.name, mode=0o700, dir_fd=self.parent_fd)
            except FileExistsError:
                pass
            else:
                if os.geteuid() == 0:
                    os.chown(self.name, self.uid, self.gid, dir_fd=self.parent_fd, follow_symlinks=False)
                os.fsync(self.parent_fd)
        self.directory_fd = os.open(self.name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=self.parent_fd)
        self.fds.append(self.directory_fd)
        self.directory_info = os.fstat(self.directory_fd)
        if stat.S_IMODE(self.directory_info.st_mode) != 0o700 or (self.directory_info.st_uid, self.directory_info.st_gid) != (self.uid, self.gid):
            raise Refusal("private deployment state requires trusted installation")
        self.recheck()

    def open_trusted_directory(self, path):
        fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
        self.fds.append(fd)
        for component in path.parts[1:]:
            fd = os.open(component, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=fd)
            self.fds.append(fd)
            info = os.fstat(fd)
            if info.st_uid not in (0, os.geteuid(), self.trusted_uid) or info.st_mode & 0o022:
                raise Refusal("untrusted deployment state ancestry")
        return fd

    def recheck(self):
        current = os.stat(self.name, dir_fd=self.parent_fd, follow_symlinks=False)
        if metadata(current)[:6] != metadata(self.directory_info)[:6] or not stat.S_ISDIR(current.st_mode):
            raise Refusal("deployment state directory replaced")
        lock = os.stat(Path(self.parent) / ("deploy-transaction.lock" if self.name == "voice-restart-associations" else ".deploy-transaction.lock"), follow_symlinks=False)
        if metadata(lock)[:6] != metadata(self.lock_info)[:6]:
            raise Refusal("deployment lock replaced")
        # Reopen ancestry to reject a renamed/replaced canonical parent.
        canonical = self.open_trusted_directory(self.parent)
        if namespace_identity(canonical) != namespace_identity(self.parent_fd):
            raise Refusal("deployment parent replaced")

    def record_name(self, project, slot):
        if not isinstance(project, str) or not PROJECT.fullmatch(project) or slot not in SLOTS:
            raise Refusal("invalid record key")
        return hashlib.sha256(project.encode("utf-8")).hexdigest() + "." + slot + ".json"

    def read(self, project, slot, missing_ok=False):
        self.recheck()
        name = self.record_name(project, slot)
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, dir_fd=self.directory_fd)
        except FileNotFoundError:
            if missing_ok:
                return None
            raise
        try:
            before = os.fstat(fd)
            if not stat.S_ISREG(before.st_mode) or stat.S_IMODE(before.st_mode) != 0o600 or before.st_uid != self.uid or before.st_nlink != 1 or before.st_size > 4096:
                raise Refusal("unsafe stopped association file")
            raw = b""
            while len(raw) <= 4096:
                chunk = os.read(fd, 4097 - len(raw))
                if not chunk:
                    break
                raw += chunk
            after = os.fstat(fd)
            named = os.stat(name, dir_fd=self.directory_fd, follow_symlinks=False)
            if metadata(before) != metadata(after) or metadata(after) != metadata(named) or len(raw) != before.st_size:
                raise Refusal("stopped association changed during read")
            value = association_value(raw)
            if value["composeProject"] != project or value["slot"] != slot:
                raise Refusal("record key differs")
            return value, metadata(after)
        finally:
            os.close(fd)

    def publish(self, value):
        project, slot = value["composeProject"], value["slot"]
        old = self.read(project, slot, missing_ok=True)
        raw = json.dumps(value, separators=(",", ":")).encode("utf-8")
        association_value(raw)
        name = self.record_name(project, slot)
        temporary = ".association-" + os.urandom(16).hex()
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=self.directory_fd)
        try:
            if os.geteuid() == 0:
                os.fchown(fd, self.uid, -1)
            before = os.fstat(fd)
            if stat.S_IMODE(before.st_mode) != 0o600 or before.st_uid != self.uid or before.st_nlink != 1:
                raise Refusal("unsafe publication file")
            view = memoryview(raw)
            while view:
                count = os.write(fd, view)
                if count <= 0:
                    raise Refusal("incomplete association write")
                view = view[count:]
            os.fsync(fd)
            self.recheck()
            if self.read(project, slot, missing_ok=True) != old:
                raise Refusal("publication target changed")
            os.rename(temporary, name, src_dir_fd=self.directory_fd, dst_dir_fd=self.directory_fd)
            os.fsync(self.directory_fd)
        finally:
            os.close(fd)
            try:
                os.unlink(temporary, dir_fd=self.directory_fd)
            except FileNotFoundError:
                pass

    def consume(self, value, identity, slot, project, allow_dead=False):
        record, observed = self.read(project, slot)
        if record != value:
            raise Refusal("pending association changed")
        match_association(value, identity, slot, project, allow_dead)
        self.recheck()
        named = os.stat(self.record_name(project, slot), dir_fd=self.directory_fd, follow_symlinks=False)
        if metadata(named) != observed:
            raise Refusal("association replaced before consumption")
        os.unlink(self.record_name(project, slot), dir_fd=self.directory_fd)
        os.fsync(self.directory_fd)

    def close(self):
        for fd in reversed(self.fds):
            os.close(fd)
        self.fds.clear()


class ContainerObservation:
    def __init__(self, identity, slot, project):
        if not DIGEST.fullmatch(identity) or slot not in SLOTS or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", project):
            raise Refusal("invalid configured identity")
        self.identity, self.slot, self.project = identity, slot, project
        self.fds = []
        self.physical_error = None
        self.before = inspect_container(identity)
        self.validate_membership(self.before)
        state = self.before["State"]
        if state.get("Status") != "running" or state.get("Restarting") or state.get("Paused") or type(state.get("Pid")) is not int or state["Pid"] <= 1:
            raise Refusal("capture requires stable running container")
        self.pid = state["Pid"]
        self.started = state["StartedAt"]
        timestamp(self.started)
        self.image = self.before.get("Image", "")
        if not runtime_time(self.before.get("Created")) <= runtime_time(self.started) or type(self.before.get("RestartCount")) is not int or self.before["RestartCount"] < 0:
            raise Refusal("invalid live runtime history")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", self.image):
            raise Refusal("unknown runtime image")
        try:
            self.capture_descriptors()
        except (OSError, ValueError, KeyError, Refusal) as error:
            # Graceful service retirement remains available on other platforms.
            # This owner will never provide physical evidence after this failure.
            self.physical_error = error
        port = "3000"
        for entry in self.before.get("Config", {}).get("Env", []):
            if entry.startswith("PORT="):
                port = entry[5:]
        if not re.fullmatch(r"[0-9]{1,5}", port) or not 1 <= int(port) <= 65535:
            raise Refusal("invalid container HTTP port")
        probe = r'''const http=require('node:http');const r=http.get({host:'127.0.0.1',port:Number(process.argv[1]),path:'/readyz',timeout:2000},s=>{if(![200,503].includes(s.statusCode)){process.exitCode=1;s.resume();return;}let b='';s.on('data',c=>{b+=c;if(b.length>65536)r.destroy();});s.on('end',()=>process.stdout.write(b));});r.on('timeout',()=>r.destroy());r.on('error',()=>{process.exitCode=1;});'''
        body = run_json("docker", "exec", identity, "node", "-e", probe, port)
        self.ready_body = body
        self.boot = body.get("nodeBootId") if isinstance(body, dict) else None
        if not isinstance(self.boot, str) or not UUID.fullmatch(self.boot):
            # Protocol-3 media never reports its boot, so neither retirement
            # route can bind it; refuse while the caller has stopped nothing.
            raise OperatorRefusal(f"running {slot} reports no protocol-4 boot identity, so it cannot be retired; "
                                  "refusing before stop (media left running)")
        self.same_runtime(inspect_container(identity), running=True)
        if self.physical_error is None:
            if process_start(self.pid) != self.start_identity:
                raise Refusal("init changed during capture")
            with open(f"/proc/{self.pid}/ns/pid", "rb") as current:
                if namespace_identity(current.fileno()) != self.target_namespace:
                    raise Refusal("namespace changed during capture")

    def open_fd(self, path, flags=os.O_RDONLY):
        fd = os.open(path, flags | os.O_CLOEXEC)
        self.fds.append(fd)
        return fd

    def validate_membership(self, value):
        labels = value.get("Config", {}).get("Labels", {})
        if labels.get("com.docker.compose.project") != self.project or labels.get("com.docker.compose.service") != self.slot or labels.get("com.docker.compose.oneoff", "False").lower() != "false" or slot_owners(self.project, self.slot) != [self.identity]:
            raise Refusal("ambiguous configured slot")

    def same_runtime(self, value, running=False):
        self.validate_membership(value)
        state = value["State"]
        if value.get("Created") != self.before.get("Created") or value.get("Image") != self.image or state.get("StartedAt") != self.started or value.get("RestartCount") != self.before.get("RestartCount"):
            raise Refusal("runtime replaced or restarted")
        if state.get("Restarting") or state.get("Paused"):
            raise Refusal("runtime unstable")
        if running and (state.get("Status") != "running" or state.get("Pid") != self.pid):
            raise Refusal("capture changed")

    def capture_descriptors(self):
        if sys.platform != "linux" or os.geteuid() != 0:
            raise Refusal("physical probe requires Linux host root")
        context = run_json("docker", "context", "inspect")
        endpoint = context[0]["Endpoints"]["docker"]["Host"]
        if not endpoint.startswith("unix://") or not stat.S_ISSOCK(os.stat(endpoint[7:]).st_mode):
            raise Refusal("Docker daemon is not local")
        info = run_json("docker", "info", "--format", "{{json .}}")
        if info.get("OSType") != "linux" or "desktop" in info.get("OperatingSystem", "").lower() or any("rootless" in option for option in info.get("SecurityOptions", [])) or str(info.get("CgroupVersion")) != "2":
            raise Refusal("unsupported Docker host")
        host = self.before.get("HostConfig", {})
        if host.get("PidMode") not in ("", "private") or host.get("Privileged") or host.get("CapAdd"):
            raise Refusal("unsupported namespace privileges")
        self.host_fd = self.open_fd("/proc/1/ns/pid")
        self.host_namespace = namespace_identity(self.host_fd)
        own = self.open_fd("/proc/self/ns/pid")
        if namespace_identity(own) != self.host_namespace:
            raise Refusal("not in host PID namespace")
        self.start_identity = process_start(self.pid)
        status = Path(f"/proc/{self.pid}/status").read_text(encoding="ascii")
        namespaces = [line.split()[1:] for line in status.splitlines() if line.startswith("NSpid:")]
        if len(namespaces) != 1 or len(namespaces[0]) < 2 or namespaces[0][-1] != "1":
            raise Refusal("container init not visible from host")
        self.target_fd = self.open_fd(f"/proc/{self.pid}/ns/pid")
        self.target_namespace = namespace_identity(self.target_fd)
        if self.target_namespace == self.host_namespace:
            raise Refusal("shared host namespace")
        entries = Path(f"/proc/{self.pid}/cgroup").read_text(encoding="ascii").splitlines()
        unified = [entry[3:] for entry in entries if entry.startswith("0::/")]
        if len(unified) != 1 or self.identity not in unified[0]:
            raise Refusal("local host cgroup/container association unavailable")
        mounts = Path("/proc/self/mountinfo").read_text(encoding="ascii").splitlines()
        groups = [line.split(" - ", 1)[0].split() for line in mounts if " - cgroup2 " in line]
        procs = [line.split(" - ", 1)[0].split() for line in mounts if " - proc " in line and line.split()[4] == "/proc"]
        if len(groups) != 1 or groups[0][3] != "/" or len(procs) != 1 or procs[0][3] != "/" or "hidepid=" in procs[0][5]:
            raise Refusal("incomplete host procfs/cgroup mount")
        root = Path(groups[0][4]).resolve(strict=True)
        self.cgroup_path = (root / unified[0].lstrip("/")).resolve(strict=True)
        if not self.cgroup_path.is_relative_to(root):
            raise Refusal("cgroup outside host mount")
        self.group_fd = self.open_fd(self.cgroup_path, os.O_RDONLY | os.O_DIRECTORY)
        self.events_fd = os.open("cgroup.events", os.O_RDONLY | os.O_CLOEXEC, dir_fd=self.group_fd)
        self.fds.append(self.events_fd)
        self.group_identity = namespace_identity(self.group_fd)
        self.events_identity = namespace_identity(self.events_fd)

    def stopped_container(self):
        after = inspect_container(self.identity)
        self.same_runtime(after)
        state = after["State"]
        if state.get("Status") not in ("exited", "dead") or state.get("Running") is not False or state.get("Pid") != 0 or runtime_time(state.get("FinishedAt")) <= runtime_time(self.started):
            raise Refusal("container is not definitely stopped")
        return state["FinishedAt"]

    def namespace_empty(self):
        before = {entry.name for entry in Path("/proc").iterdir() if entry.name.isdecimal()}
        for pid in before:
            fd = None
            try:
                start = process_start(pid)
                fd = os.open(f"/proc/{pid}/ns/pid", os.O_RDONLY | os.O_CLOEXEC)
                seen = set()
                while True:
                    identity = namespace_identity(fd)
                    if identity == self.target_namespace:
                        raise Refusal("process survives in target namespace or descendant")
                    if identity == self.host_namespace:
                        break
                    if identity in seen:
                        raise Refusal("namespace ancestry cycle")
                    seen.add(identity)
                    parent = fcntl.ioctl(fd, NS_GET_PARENT)
                    os.close(fd)
                    fd = parent
                if process_start(pid) != start:
                    return False
            except FileNotFoundError:
                if Path(f"/proc/{pid}").exists():
                    raise Refusal("unclassified process")
                return False
            finally:
                if fd is not None:
                    os.close(fd)
        after = {entry.name for entry in Path("/proc").iterdir() if entry.name.isdecimal()}
        return before == after

    def verify_physical(self, deadline):
        finished = self.stopped_container()
        if self.physical_error is not None:
            raise Refusal("unsupported or incomplete captured host capability")
        if namespace_identity(self.target_fd) != self.target_namespace or namespace_identity(self.group_fd) != self.group_identity or namespace_identity(self.events_fd) != self.events_identity:
            raise Refusal("captured descriptor lost")
        try:
            if process_start(self.pid) == self.start_identity:
                raise Refusal("captured init has not exited")
        except FileNotFoundError:
            pass
        try:
            current = os.stat(self.cgroup_path)
        except FileNotFoundError:
            current = None
        if current is not None:
            if (current.st_dev, current.st_ino) != self.group_identity:
                raise Refusal("cgroup replaced")
            os.lseek(self.events_fd, 0, os.SEEK_SET)
            rows = os.read(self.events_fd, 4097)
            if len(rows) > 4096:
                raise Refusal("invalid cgroup observation")
            populated = [row.split()[1:] for row in rows.splitlines() if row.startswith(b"populated ")]
            if populated != [[b"0"]]:
                raise Refusal("cgroup not proven unpopulated")
        while not self.namespace_empty():
            if time.monotonic() >= deadline:
                raise Refusal("host enumeration unstable")
        self.stopped_container()
        return ["--node-boot-id", self.boot, "--slot", self.slot, "--container-id", self.identity,
                "--image-sha256", self.image[7:], "--started-at", self.started, "--finished-at", finished]

    def close(self):
        for fd in reversed(self.fds):
            os.close(fd)
        self.fds.clear()


def retirement_route(owner):
    """Name the route that can retire this still-running boot after its stop.

    Invariant (#2272): the controller stops a running media slot only when the
    stop can end in a published association. Either the boot advertises live
    protocol-4 transport, so its shutdown owner can commit its own retirement
    fence, or this process retained the complete host-root physical capture the
    crash fallback needs. Without either, stopping strands a slot that no later
    invocation may restart, so refuse while the media is still serving.
    """
    body = getattr(owner, "ready_body", None)
    if (isinstance(body, dict) and type(body.get("voiceEnforcementProtocol")) is int
            and body["voiceEnforcementProtocol"] == 4 and body.get("voiceEnforcementTransportOK") is True
            and body.get("nodeBootId") == owner.boot):
        return "graceful"
    if owner.physical_error is None:
        return "physical"
    raise OperatorRefusal(f"running {owner.slot} cannot self-retire (no live protocol-4 transport) and its "
                          "physical-stop retirement requires Linux host root (EUID 0) on local rootful Docker; "
                          "refusing before stop (media left running)")


# Bounded budget for the caller's post-stop verify-retired, verify-stopped and
# publish-stopped round trips; each caller read is itself bounded at 10 s.
POST_STOP_SECONDS = 60


def hold_container(args):
    store = AssociationStore(args.lock_path) if getattr(args, "lock_path", None) else None
    if store:
        store.read(args.compose_project, args.slot, missing_ok=True)
    owner = ContainerObservation(args.container_id, args.slot, args.compose_project)
    published = None
    try:
        # Fail before the boot line: the caller stops nothing until it reads it.
        retirement_route(owner)
        print(owner.boot, flush=True)
        # One bounded lifetime covers the stop grace plus the post-stop
        # verify/retire/publish round trips; enumeration never gets a second one.
        deadline = time.monotonic() + args.timeout_seconds + POST_STOP_SECONDS
        verified = False
        while True:
            remaining = None if verified else deadline - time.monotonic()
            if (remaining is not None and remaining <= 0) or not select.select([sys.stdin], [], [], remaining)[0]:
                raise Refusal("observation deadline exhausted")
            command = sys.stdin.readline(64)
            if command == "release\n":
                owner.stopped_container()
                if store and (published is None or stopped_identity(owner.identity, owner.slot, owner.project, True) != published):
                    raise Refusal("published runtime changed before release")
                return
            if command == "publish-stopped\n" and store and published is None:
                owner.stopped_container()
                current = stopped_identity(owner.identity, owner.slot, owner.project, True)
                value = dict(schema="concord.stopped-media-association", version=1,
                             nodeBootId=owner.boot, **current)
                store.publish(value)
                published = current
                print(json.dumps(value, separators=(",", ":")), flush=True)
                continue
            if command != "verify-stopped\n":
                raise Refusal("observation owner disconnected")
            if verified:
                raise Refusal("physical witness already emitted")
            values = owner.verify_physical(deadline)
            verified = True
            print("\n".join(values), flush=True)
    finally:
        owner.close()
        if store:
            store.close()


def image_artifact(identity):
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", identity):
        raise Refusal("invalid immutable image identity")
    process = subprocess.Popen(["docker", "image", "save", identity], stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL)
    digest = hashlib.sha256()
    try:
        while True:
            chunk = process.stdout.read(1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        if process.wait() != 0:
            raise Refusal("incomplete image artifact export")
        print(digest.hexdigest())
    finally:
        process.stdout.close()
        if process.poll() is None:
            process.kill()
            process.wait()


def configured_inventory(raw):
    value = decode(raw, 2 * 1024 * 1024)
    if not isinstance(value, dict) or not isinstance(value.get("services"), dict):
        raise Refusal("rendered inventory unavailable")
    services = value["services"]
    project = value.get("name")
    if not isinstance(project, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", project):
        raise Refusal("invalid Compose project")
    control = services.get("control-plane", {}).get("environment", {})
    placement = control.get("ROOM_PLACEMENT_ENABLED", "false")
    if placement not in ("true", "false"):
        raise Refusal("unknown configured placement")
    slots = sorted(set(services) & SLOTS)
    expected = ["media-plane-blue", "media-plane-green"] if placement == "true" else ["media-plane"]
    if slots != expected:
        raise Refusal("partial, mixed or unsupported inventory")
    for slot in slots:
        service = services[slot]
        if service.get("scale", 1) != 1 or service.get("deploy", {}).get("replicas", 1) != 1:
            raise Refusal("replicated media slot")
        if service.get("environment", {}).get("ROOM_PLACEMENT_ENABLED", "false") != placement:
            raise Refusal("media/control placement mismatch")
    return project, slots


def activation_receipt(raw, control_digest, slots):
    value = strict_status(raw)
    if value["familyActivation"] != "prepared" or not DIGEST.fullmatch(control_digest):
        raise Refusal("activation is not prepared")
    if any(value[key] for key in COUNTS - RETAINED - {"legacyUsableSIDs"}):
        raise Refusal("activation liabilities remain")
    entries = []
    for slot, digest, boot in slots:
        if slot not in SLOTS or not DIGEST.fullmatch(digest) or not UUID.fullmatch(boot):
            raise Refusal("invalid slot receipt")
        entries.append({"slot": slot, "artifactSHA256": digest, "nodeBootId": boot})
    names = sorted(entry["slot"] for entry in entries)
    if names not in (["media-plane"], ["media-plane-blue", "media-plane-green"]):
        raise Refusal("incomplete activation inventory")
    import uuid
    receipt = {"schema": "concord.login-family-activation", "version": 1,
               "activationId": str(uuid.uuid4()), "protocol": 4,
               "controlPlaneArtifactSHA256": control_digest, "mediaSlots": entries}
    receipt.update({key: value[key] if key in RETAINED else 0 for key in COUNTS})
    return receipt


def native_artifact(directory):
    root = Path(directory).resolve(strict=True)
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*"), key=lambda item: item.relative_to(root).as_posix()):
        name = path.relative_to(root).as_posix().encode("utf-8")
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode):
            if not path.resolve(strict=True).is_relative_to(root):
                raise Refusal("external runtime symlink")
            kind, content = b"link", os.readlink(path).encode("utf-8")
        elif stat.S_ISDIR(info.st_mode):
            kind, content = b"dir", b""
        elif stat.S_ISREG(info.st_mode):
            kind, content = b"file", None
        else:
            raise Refusal("special runtime file")
        for part in (kind, name, str(stat.S_IMODE(info.st_mode)).encode("ascii")):
            digest.update(len(part).to_bytes(8, "big"))
            digest.update(part)
        size = info.st_size if content is None else len(content)
        digest.update(size.to_bytes(8, "big"))
        if content is not None:
            digest.update(content)
        else:
            consumed = 0
            with path.open("rb") as stream:
                while True:
                    chunk = stream.read(1024 * 1024)
                    if not chunk:
                        break
                    digest.update(chunk)
                    consumed += len(chunk)
            if consumed != size:
                raise Refusal("incomplete runtime artifact read")
            after = path.stat()
            if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
                raise Refusal("runtime artifact changed while reading")
    return digest.hexdigest()


def stage_native(source, destination, kind):
    source = Path(source).resolve(strict=True)
    destination = Path(destination)
    # Never delete an unknown old executable closure to make a new launch fit.
    destination.mkdir(mode=0o700)
    if kind == "control":
        shutil.copy2(source / "bin/control-plane", destination / "control-plane")
    else:
        node = shutil.which("node")
        if node is None:
            raise Refusal("Node executable unavailable")
        shutil.copy2(Path(node).resolve(strict=True), destination / "node")
        for name in ("dist", "node_modules", "stubs"):
            shutil.copytree(source / name, destination / name, symlinks=True)
        for name in ("package.json", "package-lock.json"):
            shutil.copy2(source / name, destination / name)
    # Resolve every link before setting read-only modes. A failed stage stays
    # present and refuses a later launch until its ownership is resolved.
    native_artifact(destination)
    for path in destination.rglob("*"):
        if path.is_symlink():
            continue
        mode = path.stat().st_mode
        path.chmod(0o555 if path.is_dir() or mode & 0o111 else 0o444)
    destination.chmod(0o555)
    if kind == "control":
        return file_artifact(destination / "control-plane")
    return native_artifact(destination)


def file_artifact(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        before = os.fstat(stream.fileno())
        consumed = 0
        while True:
            chunk = stream.read(1024 * 1024)
            if not chunk:
                break
            consumed += len(chunk)
            digest.update(chunk)
        after = os.fstat(stream.fileno())
    if consumed != before.st_size or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
        raise Refusal("incomplete or changing executable artifact")
    return digest.hexdigest()


def native_process(pid, directory, kind):
    if not pid.isdecimal() or int(pid) <= 1:
        raise Refusal("invalid native PID")
    expected = (Path(directory) / ("node" if kind == "media" else "control-plane")).resolve(strict=True)
    if sys.platform == "linux":
        actual = Path(f"/proc/{pid}/exe").resolve(strict=True)
        if actual != expected:
            raise Refusal("native executable association changed")
        if kind == "media":
            argv = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            if str(Path(directory).resolve() / "dist/index.js").encode() not in argv:
                raise Refusal("native script association changed")
    else:
        result = subprocess.run(["lsof", "-a", "-p", pid, "-d", "txt", "-Fn"],
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5, check=False)
        if result.returncode or f"n{expected}" not in result.stdout.decode("utf-8").splitlines():
            raise Refusal("native executable association unavailable")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="operation", required=True)
    status_parser = commands.add_parser("status")
    status_parser.add_argument("--field", choices=sorted(STATUS_FIELDS))
    status_parser.add_argument("--drained", action="store_true")
    status_parser.add_argument("--allow-legacy-normalization", action="store_true")
    owner_parser = commands.add_parser("hold-container")
    owner_parser.add_argument("--lock-path")
    owner_parser.add_argument("--container-id", required=True)
    owner_parser.add_argument("--slot", required=True, choices=sorted(SLOTS))
    owner_parser.add_argument("--compose-project", required=True)
    owner_parser.add_argument("--timeout-seconds", type=int, choices=[45], required=True)
    for operation in ("association-state", "association-invalidate", "association-read", "association-consume", "association-match", "stopped-identity"):
        association_parser = commands.add_parser(operation)
        if operation not in ("association-match", "stopped-identity"):
            association_parser.add_argument("--lock-path", required=True)
        association_parser.add_argument("--compose-project", required=True)
        association_parser.add_argument("--slot", required=True, choices=sorted(SLOTS | ({"control-plane"} if operation == "stopped-identity" else set())))
        if operation not in ("association-state", "association-invalidate"):
            association_parser.add_argument("--container-id", required=True)
            association_parser.add_argument("--allow-dead", action="store_true")
    image_parser = commands.add_parser("image-artifact")
    image_parser.add_argument("image_id")
    commands.add_parser("inventory")
    native_ready = commands.add_parser("native-ready")
    native_ready.add_argument("--grant", choices=["boot-only", "true", "false", "either"], required=True)
    file_parser = commands.add_parser("file-artifact")
    file_parser.add_argument("path")
    preflight_parser = commands.add_parser("retirement-preflight")
    preflight_parser.add_argument("--container-id", required=True)
    preflight_parser.add_argument("--slot", choices=sorted(SLOTS), required=True)
    preflight_parser.add_argument("--compose-project", required=True)
    probe_parser = commands.add_parser("probe-container")
    probe_parser.add_argument("--container-id", required=True)
    probe_parser.add_argument("--slot", choices=sorted(SLOTS), required=True)
    probe_parser.add_argument("--compose-project", required=True)
    probe_parser.add_argument("--grant", choices=["true", "false", "either"], default="either")
    stage_parser = commands.add_parser("stage-native")
    stage_parser.add_argument("--source", required=True)
    stage_parser.add_argument("--destination", required=True)
    stage_parser.add_argument("--kind", choices=["media", "control"], required=True)
    native_parser = commands.add_parser("native-artifact")
    native_parser.add_argument("directory")
    process_parser = commands.add_parser("native-process")
    process_parser.add_argument("--process-id", required=True)
    process_parser.add_argument("--directory", required=True)
    process_parser.add_argument("--kind", choices=["media", "control"], required=True)
    receipt_parser = commands.add_parser("receipt")
    receipt_parser.add_argument("--control-plane-artifact", required=True)
    receipt_parser.add_argument("--slot", nargs=3, action="append", required=True)
    args = parser.parse_args()
    try:
        if args.operation == "stopped-identity":
            print(json.dumps(stopped_identity(args.container_id, args.slot, args.compose_project, args.allow_dead), separators=(",", ":")))
        elif args.operation == "association-match":
            value = association_value(sys.stdin.buffer.read(4097))
            match_association(value, args.container_id, args.slot, args.compose_project, args.allow_dead)
        elif args.operation in ("association-state", "association-invalidate", "association-read", "association-consume"):
            store = AssociationStore(args.lock_path)
            try:
                if args.operation == "association-state":
                    store.read(args.compose_project, args.slot, missing_ok=True)
                    os.fsync(store.directory_fd)
                elif args.operation == "association-invalidate":
                    found = store.read(args.compose_project, args.slot, missing_ok=True)
                    if found:
                        store.recheck()
                        if metadata(os.stat(store.record_name(args.compose_project, args.slot), dir_fd=store.directory_fd, follow_symlinks=False)) != found[1]:
                            raise Refusal("stale association changed")
                        os.unlink(store.record_name(args.compose_project, args.slot), dir_fd=store.directory_fd)
                        os.fsync(store.directory_fd)
                elif args.operation == "association-read":
                    value, _ = store.read(args.compose_project, args.slot)
                    match_association(value, args.container_id, args.slot, args.compose_project, args.allow_dead)
                    print(json.dumps(value, separators=(",", ":")))
                else:
                    value = association_value(sys.stdin.buffer.read(4097))
                    store.consume(value, args.container_id, args.slot, args.compose_project, args.allow_dead)
            finally:
                store.close()
        elif args.operation == "status":
            value = strict_status(sys.stdin.buffer.read(65537))
            liabilities = COUNTS - RETAINED
            if args.allow_legacy_normalization:
                liabilities -= {"legacyUsableSIDs"}
            if args.drained and any(value[key] != 0 for key in liabilities):
                raise Refusal("live or pending liabilities remain")
            if args.field:
                field = value[args.field]
                print(json.dumps(field) if field is None or type(field) is bool else field)
            else:
                print(json.dumps(value, separators=(",", ":")))
        elif args.operation == "native-ready":
            body = decode(sys.stdin.buffer.read(65537))
            if not isinstance(body, dict) or not isinstance(body.get("nodeBootId"), str) or not UUID.fullmatch(body["nodeBootId"]) or type(body.get("voiceEnforcementProtocol")) is not int or body["voiceEnforcementProtocol"] != 4 or "colour" in body:
                raise Refusal("unsupported native slot identity")
            if args.grant != "boot-only":
                if body.get("voiceEnforcementTransportOK") is not True or type(body.get("voiceEnforcementAdmissionGranted")) is not bool or type(body.get("ready")) is not bool:
                    raise Refusal("native protocol4 transport unavailable")
                if args.grant in ("true", "false") and (body["voiceEnforcementAdmissionGranted"] is not (args.grant == "true") or body["ready"] is not (args.grant == "true")):
                    raise Refusal("native admission differs from expected state")
            print(body["nodeBootId"])
        elif args.operation == "file-artifact":
            print(file_artifact(args.path))
        elif args.operation == "stage-native":
            print(stage_native(args.source, args.destination, args.kind))
        elif args.operation == "native-artifact":
            print(native_artifact(args.directory))
        elif args.operation == "native-process":
            native_process(args.process_id, args.directory, args.kind)
        elif args.operation == "probe-container":
            owner = ContainerObservation(args.container_id, args.slot, args.compose_project)
            try:
                body = owner.ready_body
                if type(body.get("voiceEnforcementProtocol")) is not int or body["voiceEnforcementProtocol"] != 4 or body.get("voiceEnforcementTransportOK") is not True or type(body.get("voiceEnforcementAdmissionGranted")) is not bool or type(body.get("ready")) is not bool:
                    raise Refusal("slot protocol4 transport unavailable")
                if args.grant == "true" and (body["voiceEnforcementAdmissionGranted"] is not True or body["ready"] is not True):
                    raise Refusal("slot admission unavailable")
                if args.grant == "false" and (body["voiceEnforcementAdmissionGranted"] is not False or body["ready"] is not False):
                    raise Refusal("prepared slot admitted clients")
                print(owner.boot)
                print(owner.image)
            finally:
                owner.close()
        elif args.operation == "retirement-preflight":
            owner = ContainerObservation(args.container_id, args.slot, args.compose_project)
            try:
                print(retirement_route(owner))
            finally:
                owner.close()
        elif args.operation == "inventory":
            project, slots = configured_inventory(sys.stdin.buffer.read(2 * 1024 * 1024 + 1))
            print("\n".join([project] + slots))
        elif args.operation == "receipt":
            print(json.dumps(activation_receipt(sys.stdin.buffer.read(65537), args.control_plane_artifact, args.slot), separators=(",", ":")))
        elif args.operation == "hold-container":
            hold_container(args)
        else:
            image_artifact(args.image_id)
    except OperatorRefusal as error:
        print(f"voice lifecycle: {error}", file=sys.stderr)
        return 1
    except Refusal as error:
        # Every plain Refusal reason is a fixed literal, so naming it is safe.
        print(f"voice lifecycle: observation refused: {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError):
        print("voice lifecycle: observation refused", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
