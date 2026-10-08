#!/usr/bin/env python3
"""Read-only admission and bounded loopback TLS observation for self-host coturn."""
from __future__ import annotations

import json
import os
from pathlib import Path
import posixpath
import re
import signal
import socket
import ssl
import stat
import subprocess
import sys
import time


IMAGE = "coturn/coturn:4.6.3-alpine"
CERT_ROOT = "/opt/concord/certs/coturn"
CERT_TARGET = "/etc/coturn/certs"
CERT_PATH = CERT_TARGET + "/cert.pem"
KEY_PATH = CERT_TARGET + "/key.pem"
TLS_PORT = 5349
TOTAL_LIMIT = 15.0
ATTEMPT_LIMIT = 3.0
RETRY_DELAY = 0.1
CERT_FILE_LIMIT = 4 * 1024 * 1024
HANDSHAKE_REFUSAL = "self-host TLS handshake refused"
LEAF_MISMATCH_REFUSAL = "self-host TLS leaf mismatch refused"
DEADLINE_REFUSAL = "self-host TLS deadline exceeded"
CONFIG_SOURCE_SUFFIX = "infrastructure/docker/coturn/turnserver.conf"
CONFIG_TARGET = "/etc/coturn/turnserver.conf"
INSPECT_TEMPLATE = (
    '{"Config":{"Image":{{json .Config.Image}},"User":{{json .Config.User}},'
    '"Labels":{"com.docker.compose.project":{{json (index .Config.Labels "com.docker.compose.project")}},'
    '"com.docker.compose.service":{{json (index .Config.Labels "com.docker.compose.service")}}},'
    '"Cmd":{{json .Config.Cmd}}},"State":{"Status":{{json .State.Status}},'
    '"Running":{{json .State.Running}},"Paused":{{json .State.Paused}},'
    '"Restarting":{{json .State.Restarting}}},"Mounts":{{json .Mounts}},'
    '"NetworkSettings":{"Ports":{{json .NetworkSettings.Ports}}}}'
)


class TLSRefusalError(Exception):
    """Fixed, sanitized refusal class for expected admission and observation failures."""


def _refuse(reason: str) -> None:
    raise TLSRefusalError(reason)


def _need(condition: object, reason: str) -> None:
    if not condition:
        _refuse(reason)


def _required(mapping: object, key: str, reason: str):
    _need(isinstance(mapping, dict) and key in mapping, reason)
    return mapping[key]


def _valid_domain(value: object) -> bool:
    return (isinstance(value, str)
            and re.fullmatch(r"([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}", value) is not None
            and not value.startswith(("media.", "api.", "turn.")))


def _mount_beneath(mount: object, field: str, target: str) -> bool:
    if not isinstance(mount, dict):
        return False
    value = mount.get(field)
    if not isinstance(value, str) or not posixpath.isabs(value):
        return False
    normalized = posixpath.normpath(value)
    return normalized == target or normalized.startswith(target.rstrip("/") + "/")


def _mounts_beneath(mounts: object, field: str, target: str) -> list[dict]:
    _need(isinstance(mounts, list), "self-host TLS mount refused")
    return [mount for mount in mounts if _mount_beneath(mount, field, target)]


def validate_model(model):
    """Admit the ordinary rendered coturn consumer without inspecting secrets."""
    try:
        services = _required(model, "services", "self-host TLS model refused")
        service = _required(services, "coturn", "self-host TLS model refused")
        _need(_required(service, "image", "self-host TLS model refused") == IMAGE,
              "self-host TLS model refused")
        _need(service.get("user") in (None, "") and service.get("entrypoint") in (None, []),
              "self-host TLS model refused")
        command = _required(service, "command", "self-host TLS model refused")
        _need(isinstance(command, list) and all(isinstance(arg, str) for arg in command),
              "self-host TLS model refused")
        _need(command and command[0] == "turnserver" and
              _command_values(command, "-c") == [CONFIG_TARGET],
              "self-host TLS model refused")
        _need(_command_values(command, "--cert") == [CERT_PATH]
              and _command_values(command, "--pkey") == [KEY_PATH],
              "self-host TLS model refused")
        volumes = _required(service, "volumes", "self-host TLS model refused")
        _need(isinstance(volumes, list), "self-host TLS model refused")
        cert_mounts = _mounts_beneath(volumes, "target", CERT_TARGET)
        _need(len(cert_mounts) == 1, "self-host TLS model refused")
        mount = cert_mounts[0]
        _need(mount.get("target") == CERT_TARGET and mount.get("type") == "bind"
              and mount.get("source") == CERT_ROOT
              and mount.get("read_only") is True, "self-host TLS model refused")
        config_mounts = _mounts_beneath(volumes, "target", CONFIG_TARGET)
        _need(len(config_mounts) == 1, "self-host TLS model refused")
        config_mount = config_mounts[0]
        _need(config_mount.get("target") == CONFIG_TARGET and config_mount.get("type") == "bind"
              and config_mount.get("read_only") is True
              and isinstance(config_mount.get("source"), str)
              and posixpath.isabs(config_mount["source"]), "self-host TLS model refused")
        ports = _required(service, "ports", "self-host TLS model refused")
        _need(isinstance(ports, list), "self-host TLS model refused")
        tls_ports = [port for port in ports if isinstance(port, dict)
                     and port.get("target") == TLS_PORT and port.get("protocol", "tcp") == "tcp"]
        _need(len(tls_ports) == 1, "self-host TLS model refused")
        port = tls_ports[0]
        _need(str(port.get("published", "")) == str(TLS_PORT)
              and port.get("host_ip", "") in ("", "0.0.0.0"),
              "self-host TLS model refused")
        return None
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS model refused")


def _command_values(command: list[str], option: str) -> list[str]:
    values = []
    index = 0
    while index < len(command):
        argument = command[index]
        if argument.startswith(option + "="):
            values.append(argument[len(option) + 1:])
        elif argument == option:
            values.append(command[index + 1] if index + 1 < len(command) else "")
            index += 1
        index += 1
    return values


def _lstat(path: Path, reason: str):
    try:
        return path.lstat()
    except OSError:
        _refuse(reason)


def _check_directory(path: Path, mode: int) -> None:
    info = _lstat(path, "self-host TLS material refused")
    _need(stat.S_ISDIR(info.st_mode) and info.st_uid == 0 and info.st_gid == 0
          and stat.S_IMODE(info.st_mode) == mode, "self-host TLS material refused")


def validate_metadata(cert_root):
    """Check certificate metadata by lstat only; never open or read the key."""
    try:
        root = Path(cert_root)
        _need(root.is_absolute(), "self-host TLS material refused")
        _check_directory(root.parent, 0o711)
        _check_directory(root, 0o711)
        cert = root / "cert.pem"
        key = root / "key.pem"
        cert_info = _lstat(cert, "self-host TLS material refused")
        key_info = _lstat(key, "self-host TLS material refused")
        _need(stat.S_ISREG(cert_info.st_mode) and cert_info.st_size > 0
              and cert_info.st_uid == 0 and cert_info.st_gid == 0
              and stat.S_IMODE(cert_info.st_mode) == 0o444,
              "self-host TLS material refused")
        _need(stat.S_ISREG(key_info.st_mode) and key_info.st_size > 0
              and key_info.st_uid == 65534 and key_info.st_gid == 65533
              and stat.S_IMODE(key_info.st_mode) == 0o400,
              "self-host TLS material refused")
        return None
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS material refused")


def validate_container(container, project):
    """Admit one selectively inspected, live container from the selected project."""
    try:
        config = _required(container, "Config", "self-host TLS consumer refused")
        labels = _required(config, "Labels", "self-host TLS consumer refused")
        _need(_required(labels, "com.docker.compose.project", "self-host TLS consumer refused") == project
              and _required(labels, "com.docker.compose.service", "self-host TLS consumer refused") == "coturn",
              "self-host TLS consumer refused")
        _need(_required(config, "Image", "self-host TLS consumer refused") == IMAGE,
              "self-host TLS consumer refused")
        _need(_required(config, "User", "self-host TLS consumer refused") in
              ("nobody:nogroup", "65534:65533"), "self-host TLS consumer refused")
        command = _required(config, "Cmd", "self-host TLS consumer refused")
        _need(isinstance(command, list) and all(isinstance(arg, str) for arg in command)
              and command and command[0] == "turnserver"
              and _command_values(command, "-c") == [CONFIG_TARGET]
              and _command_values(command, "--cert") == [CERT_PATH]
              and _command_values(command, "--pkey") == [KEY_PATH],
              "self-host TLS consumer refused")
        state = _required(container, "State", "self-host TLS consumer refused")
        _need(_required(state, "Status", "self-host TLS consumer refused") == "running"
              and _required(state, "Running", "self-host TLS consumer refused") is True
              and _required(state, "Paused", "self-host TLS consumer refused") is False
              and _required(state, "Restarting", "self-host TLS consumer refused") is False,
              "self-host TLS consumer refused")
        mounts = _required(container, "Mounts", "self-host TLS consumer refused")
        selected = _mounts_beneath(mounts, "Destination", CERT_TARGET)
        _need(len(selected) == 1, "self-host TLS consumer refused")
        mount = selected[0]
        _need(mount.get("Destination") == CERT_TARGET and mount.get("Type") == "bind"
              and mount.get("Source") == CERT_ROOT
              and mount.get("RW") is False, "self-host TLS consumer refused")
        config_mounts = _mounts_beneath(mounts, "Destination", CONFIG_TARGET)
        _need(len(config_mounts) == 1, "self-host TLS consumer refused")
        config_mount = config_mounts[0]
        _need(config_mount.get("Destination") == CONFIG_TARGET
              and config_mount.get("Type") == "bind" and config_mount.get("RW") is False
              and isinstance(config_mount.get("Source"), str)
              and posixpath.isabs(config_mount["Source"]), "self-host TLS consumer refused")
        network = _required(container, "NetworkSettings", "self-host TLS consumer refused")
        published = _required(network, "Ports", "self-host TLS consumer refused")
        _need(isinstance(published, dict), "self-host TLS consumer refused")
        bindings = _required(published, "5349/tcp", "self-host TLS consumer refused")
        _need(isinstance(bindings, list) and len(bindings) > 0,
              "self-host TLS consumer refused")
        _need(all(isinstance(binding, dict)
                  and binding.get("HostPort") == str(TLS_PORT)
                  and binding.get("HostIp") in ("", "0.0.0.0", "::")
                  for binding in bindings)
              and any(binding.get("HostIp") in ("", "0.0.0.0") for binding in bindings),
              "self-host TLS consumer refused")
        return None
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS consumer refused")


def _read_public_leaf(path: str) -> tuple[bytes, str]:
    """Read only the public certificate and return its first PEM certificate."""
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        descriptor = os.open(path, flags)
        try:
            info = os.fstat(descriptor)
            _need(stat.S_ISREG(info.st_mode) and 0 < info.st_size <= CERT_FILE_LIMIT,
                  "self-host TLS certificate refused")
            with os.fdopen(descriptor, "rb", closefd=False) as stream:
                data = stream.read(CERT_FILE_LIMIT + 1)
        finally:
            os.close(descriptor)
        _need(len(data) <= CERT_FILE_LIMIT, "self-host TLS certificate refused")
        begin = b"-----BEGIN CERTIFICATE-----"
        end = b"-----END CERTIFICATE-----"
        first = data.find(begin)
        last = data.find(end, first + len(begin)) if first >= 0 else -1
        _need(first >= 0 and last >= 0, "self-host TLS certificate refused")
        pem_bytes = data[first:last + len(end)] + b"\n"
        pem = pem_bytes.decode("ascii")
        der = ssl.PEM_cert_to_DER_cert(pem)
        _need(isinstance(der, bytes) and len(der) > 0, "self-host TLS certificate refused")
        return der, pem
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS certificate refused")


def _tls_context(first_leaf_pem: str) -> ssl.SSLContext:
    try:
        _need(hasattr(ssl, "VERIFY_X509_PARTIAL_CHAIN")
              and hasattr(ssl.SSLContext, "hostname_checks_common_name"),
              "self-host TLS prerequisite unavailable")
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        context.verify_mode = ssl.CERT_REQUIRED
        context.check_hostname = True
        context.hostname_checks_common_name = False
        context.verify_flags |= ssl.VERIFY_X509_PARTIAL_CHAIN
        context.load_verify_locations(cadata=first_leaf_pem)
        return context
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS prerequisite unavailable")


def _remaining(deadline: float, reason: str = "self-host TLS deadline exceeded") -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        _refuse(reason)
    return remaining


def _observe_tls_until(cert_path: str, domain: str, port: int, deadline: float) -> None:
    _need(_valid_domain(domain) and isinstance(port, int) and 1 <= port <= 65535,
          "self-host TLS handshake refused")
    _remaining(deadline)
    expected_der, first_leaf_pem = _read_public_leaf(cert_path)
    context = _tls_context(first_leaf_pem)
    failure_class = None
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            _refuse(failure_class or DEADLINE_REFUSAL)
        attempt_deadline = min(deadline, time.monotonic() + ATTEMPT_LIMIT)
        raw_socket = None
        tls_socket = None
        retry = False
        try:
            operation_budget = min(_remaining(deadline), attempt_deadline - time.monotonic())
            _need(operation_budget > 0, "self-host TLS deadline exceeded")
            raw_socket = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            raw_socket.settimeout(operation_budget)
            raw_socket.connect(("127.0.0.1", port))
            operation_budget = min(_remaining(deadline), attempt_deadline - time.monotonic())
            _need(operation_budget > 0, "self-host TLS deadline exceeded")
            raw_socket.settimeout(operation_budget)
            tls_socket = context.wrap_socket(raw_socket, server_hostname=domain,
                                             do_handshake_on_connect=False)
            operation_budget = min(_remaining(deadline), attempt_deadline - time.monotonic())
            _need(operation_budget > 0, "self-host TLS deadline exceeded")
            tls_socket.settimeout(operation_budget)
            tls_socket.do_handshake()
            _remaining(deadline)
            _need(time.monotonic() < attempt_deadline, "self-host TLS deadline exceeded")
            peer_der = tls_socket.getpeercert(binary_form=True)
            _need(isinstance(peer_der, bytes) and len(peer_der) > 0,
                  "self-host TLS handshake refused")
            current_der, _current_pem = _read_public_leaf(cert_path)
            if peer_der != expected_der or current_der != expected_der:
                failure_class = LEAF_MISMATCH_REFUSAL
            _remaining(deadline)
            if time.monotonic() < attempt_deadline \
                    and peer_der == expected_der and current_der == expected_der:
                return None
            retry = True
        except socket.timeout:
            if time.monotonic() >= deadline:
                _refuse(failure_class or DEADLINE_REFUSAL)
            if failure_class is None:
                failure_class = DEADLINE_REFUSAL
            retry = True
        except TLSRefusalError as error:
            if str(error) == DEADLINE_REFUSAL:
                if time.monotonic() >= deadline:
                    _refuse(failure_class or DEADLINE_REFUSAL)
                if failure_class is None:
                    failure_class = DEADLINE_REFUSAL
            elif str(error) == LEAF_MISMATCH_REFUSAL:
                failure_class = LEAF_MISMATCH_REFUSAL
            else:
                if failure_class != LEAF_MISMATCH_REFUSAL:
                    failure_class = HANDSHAKE_REFUSAL
            retry = True
        except (OSError, ssl.SSLError, ValueError):
            if failure_class != LEAF_MISMATCH_REFUSAL:
                failure_class = HANDSHAKE_REFUSAL
            retry = True
        except Exception:
            if failure_class != LEAF_MISMATCH_REFUSAL:
                failure_class = HANDSHAKE_REFUSAL
            retry = True
        finally:
            try:
                if tls_socket is not None:
                    tls_socket.close()
                elif raw_socket is not None:
                    raw_socket.close()
            except OSError:
                pass
        if retry:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                _refuse(failure_class or DEADLINE_REFUSAL)
            time.sleep(min(RETRY_DELAY, remaining))
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                _refuse(failure_class or DEADLINE_REFUSAL)
            current_expected, first_leaf_pem = _read_public_leaf(cert_path)
            if current_expected != expected_der:
                failure_class = LEAF_MISMATCH_REFUSAL
            expected_der = current_expected
            context = _tls_context(first_leaf_pem)


def observe_tls(cert_path, domain, port, budget_seconds):
    """Complete a verified TLS handshake to IPv4 loopback within one budget."""
    try:
        budget = float(budget_seconds)
        _need(0 < budget <= TOTAL_LIMIT and _valid_domain(domain)
              and isinstance(port, int) and 1 <= port <= 65535,
              "self-host TLS deadline refused")
        return _observe_tls_until(cert_path, domain, port, time.monotonic() + budget)
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS handshake refused")


def _run_bounded(argv: list[str], deadline: float, *, cwd: str | None = None,
                 env: dict[str, str] | None = None, refusal: str) -> str:
    remaining = _remaining(deadline)
    try:
        process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   text=True, start_new_session=True)
    except Exception:
        _refuse(refusal)
    try:
        stdout, _stderr = process.communicate(timeout=remaining)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except OSError:
            pass
        try:
            process.communicate(timeout=0.05)
        except subprocess.TimeoutExpired:
            for pipe in (process.stdout, process.stderr):
                if pipe is not None:
                    try:
                        pipe.close()
                    except OSError:
                        pass
            try:
                process.wait(timeout=0.05)
            except subprocess.TimeoutExpired:
                pass
        _refuse("self-host TLS deadline exceeded")
    except Exception:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except OSError:
            pass
        _refuse(refusal)
    _remaining(deadline)
    if process.returncode != 0:
        _refuse(refusal)
    return stdout


def _safe_compose_env(trusted_image: str) -> dict[str, str]:
    env = os.environ.copy()
    for name in ("SSLKEYLOGFILE", "SSL_CERT_FILE", "SSL_CERT_DIR", "CONTROL_PLANE_IMAGE",
                 "COTURN_CERTS_DIR", "COMPOSE_FILE", "COMPOSE_PROFILES", "COMPOSE_PROJECT_NAME",
                 "COMPOSE_ENV_FILES", "COMPOSE_DISABLE_ENV_FILE", "COMPOSE_OVERRIDE",
                 "SELFHOST_STORAGE_MODE", "STORAGE_BACKEND", "STORAGE_ENDPOINT", "STORAGE_REGION",
                 "STORAGE_ACCESS_KEY", "STORAGE_SECRET_KEY", "STORAGE_USE_SSL", "STORAGE_BUCKET",
                 "MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_USE_SSL",
                 "MINIO_BUCKET", "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD"):
        env.pop(name, None)
    if trusted_image:
        _need(bool(re.fullmatch(r"(?:sha256:[0-9a-fA-F]{64}|.+@sha256:[0-9a-fA-F]{64})", trusted_image)),
              "self-host TLS model refused")
        env["CONTROL_PLANE_IMAGE"] = trusted_image
    return env


def _read_tls_data(root: str, deadline: float, env: dict[str, str]) -> tuple[str, str]:
    storage_script = Path(__file__).resolve().with_name("selfhost-storage.sh")
    reader = ('source "$1"; _selfhost_storage_data read-tls "$2"')
    output = _run_bounded(["bash", "-c", reader, "selfhost-tls",
                           str(storage_script), str(Path(root) / ".env")],
                          deadline, cwd=root, env=env,
                          refusal="self-host TLS environment refused")
    try:
        value = json.loads(output)
        _need(isinstance(value, dict) and set(value) == {"domain", "cert_root"}
              and _valid_domain(value["domain"]) and value["cert_root"] == CERT_ROOT,
              "self-host TLS environment refused")
        return value["domain"], value["cert_root"]
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS environment refused")


def _compose_model(root: str, project: str, compose_args: list[str], deadline: float,
                   env: dict[str, str]) -> dict:
    output = _run_bounded(["docker", "compose", *compose_args, "config", "--format", "json"],
                          deadline, cwd=root, env=env,
                          refusal="self-host TLS model unavailable")
    try:
        model = json.loads(output)
        _need(isinstance(model, dict) and model.get("name") == project,
              "self-host TLS model refused")
        validate_model(model)
        service = model["services"]["coturn"]
        config_mounts = [mount for mount in service["volumes"]
                         if isinstance(mount, dict) and mount.get("target") == CONFIG_TARGET]
        expected_source = os.path.join(root, CONFIG_SOURCE_SUFFIX)
        _need(len(config_mounts) == 1 and config_mounts[0].get("source") == expected_source,
              "self-host TLS model refused")
        return model
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS model refused")


def _selected_container(root: str, project: str, compose_args: list[str], deadline: float,
                        env: dict[str, str]):
    output = _run_bounded(["docker", "compose", *compose_args,
                           "ps", "--all", "--quiet", "coturn"],
                          deadline, cwd=root, env=env,
                          refusal="self-host TLS consumer unavailable")
    ids = [line.strip() for line in output.splitlines() if line.strip()]
    _need(len(ids) == 1 and re.fullmatch(r"[0-9a-f]{12,64}", ids[0]) is not None,
          "self-host TLS consumer unavailable")
    inspected = _run_bounded(["docker", "inspect", "--type", "container", "--format",
                              INSPECT_TEMPLATE, ids[0]], deadline, cwd=root, env=env,
                             refusal="self-host TLS consumer unavailable")
    try:
        container = json.loads(inspected)
    except Exception:
        _refuse("self-host TLS consumer unavailable")
    validate_container(container, project)
    mounts = container["Mounts"]
    config_mounts = [mount for mount in mounts
                     if isinstance(mount, dict) and mount.get("Destination") == CONFIG_TARGET]
    expected_source = os.path.join(root, CONFIG_SOURCE_SUFFIX)
    _need(len(config_mounts) == 1 and config_mounts[0].get("Source") == expected_source,
          "self-host TLS consumer refused")
    return container


def _positive_budget(value: str) -> float:
    try:
        budget = float(value)
        _need(budget > 0, "self-host TLS caller deadline expired")
        return min(TOTAL_LIMIT, budget)
    except TLSRefusalError:
        raise
    except Exception:
        _refuse("self-host TLS caller deadline refused")


def _main(argv: list[str]) -> int:
    try:
        _need(len(argv) >= 6, "self-host TLS invocation refused")
        operation, root, project, raw_budget, trusted_image = argv[:5]
        _need(operation in ("admit", "ready"), "self-host TLS invocation refused")
        _need(argv[5] == "--" and len(argv) > 6, "self-host TLS invocation refused")
        compose_args = argv[6:]
        _need(os.path.isabs(root) and os.path.isdir(root)
              and re.fullmatch(r"[a-z0-9][a-z0-9_-]*", project) is not None,
              "self-host TLS authority refused")
        budget = _positive_budget(raw_budget)
        deadline = time.monotonic() + budget
        env = _safe_compose_env(trusted_image)
        domain, cert_root = _read_tls_data(root, deadline, env)
        validate_metadata(cert_root)
        _compose_model(root, project, compose_args, deadline, env)
        if operation == "admit":
            _remaining(deadline)
            print("self-host TLS admission ready")
            return 0
        _selected_container(root, project, compose_args, deadline, env)
        _observe_tls_until(str(Path(cert_root) / "cert.pem"), domain, TLS_PORT, deadline)
        _remaining(deadline)
        print("self-host coturn TLS ready")
        return 0
    except TLSRefusalError as error:
        print(str(error), file=sys.stderr)
        return 1
    except Exception:
        print("self-host TLS observer refused", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(_main(sys.argv[1:]))
