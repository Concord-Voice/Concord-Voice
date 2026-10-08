#!/usr/bin/env python3
"""Self-host published-port posture check (#3538).

Reads JSON on stdin and refuses any host publication outside the self-host
allowlist. EXPOSURE ONLY: it never checks that a public port IS published --
RTP/TURN completeness belongs to [internal]tests/test-rtc-port-ingress-parity.sh.

  --rendered   `docker compose ... config --format json`
  --live       `docker inspect <ids>` (a JSON array)

Exit 0 = posture OK, 1 = violation (one line per offence on stdout),
2 = input this checker cannot judge (the caller must fail closed).

Never prints an environment value: the render and Config.Env carry
.env-derived secrets (observability.md principle 1).
"""
import json
import sys

LOOPBACK = "127.0.0.1"
ALL_ADDRESSES = ("", "0.0.0.0", "::")
COTURN_PORTS = {(3478, "tcp"), (3478, "udp"), (5349, "tcp")}
COTURN_RELAY = range(49152, 49253)  # udp, inclusive of 49252
# Bridge options. Only the default `nat` gateway mode keeps a 127.0.0.1 publication off
# the LAN (`routed` opens published ports to direct routing, `nat-unprotected` opens every
# port); `isolated` has no external path at all. Trusted interfaces allow direct routing.
BRIDGE_GATEWAY_MODES = ("com.docker.network.bridge.gateway_mode_ipv4",
                        "com.docker.network.bridge.gateway_mode_ipv6")
SAFE_GATEWAY_MODES = {"nat", "isolated"}
BRIDGE_TRUSTED_IFACES = "com.docker.network.bridge.trusted_host_interfaces"


def bridge_exposes(driver, opts):
    """True when a network makes a container reachable without a 127.0.0.1-safe publication."""
    opts = opts or {}
    return (driver or "bridge") != "bridge" \
        or any((opts.get(k) or "nat") not in SAFE_GATEWAY_MODES for k in BRIDGE_GATEWAY_MODES) \
        or BRIDGE_TRUSTED_IFACES in opts


class Unjudgeable(Exception):
    """Input this checker cannot judge; never carries input text."""


def as_port(value):
    if isinstance(value, bool):
        raise Unjudgeable()
    if isinstance(value, int):
        port = value
    elif isinstance(value, str) and value.isdigit():
        port = int(value)
    else:
        raise Unjudgeable()
    if not 0 < port < 65536:
        raise Unjudgeable()
    return port


def env_map(raw):
    if raw is None:
        return {}
    if isinstance(raw, dict):
        return {str(k): v for k, v in raw.items()}
    if isinstance(raw, list):
        return dict(str(item).split("=", 1) for item in raw if "=" in str(item))
    raise Unjudgeable()


def rtc_window(env):
    try:
        low, high = as_port(env["RTC_MIN_PORT"]), as_port(env["RTC_MAX_PORT"])
    except (KeyError, Unjudgeable):
        raise Unjudgeable() from None
    if low > high:
        raise Unjudgeable()
    return low, high


def allowed(service, port, target, proto, host_ip, env):
    if port != target:  # a remapped port can expose a socket the allowlist never names
        return False
    if service == "control-plane":
        return (port, proto) == (8080, "tcp") and host_ip == LOOPBACK
    if service == "media-plane":
        if (port, proto) == (3000, "tcp"):
            return host_ip == LOOPBACK
        if proto == "udp":
            low, high = rtc_window(env)
            return low <= port <= high
        return False
    if service == "coturn":
        return (port, proto) in COTURN_PORTS or (proto == "udp" and port in COTURN_RELAY)
    return False


def label(host_ip):
    return "all" if host_ip in ALL_ADDRESSES else host_ip


def judge(service, target, proto, host_ip, published, env, problems):
    if published in (None, ""):
        problems.append(f"posture: {service} {target}/{proto} host_ip={label(host_ip)} not allowed (ephemeral host port)")
        return
    port = as_port(published)
    if not allowed(service, port, target, proto, host_ip, env):
        problems.append(f"posture: {service} {port}/{proto} host_ip={label(host_ip)} not allowed")


def network_verdicts(doc):
    """Split the render's networks into host, otherwise-exposing, and unjudgeable sets.

    A network whose engine name is "host" gives a container host networking. A
    non-bridge driver (macvlan, ipvlan, ...) or an unprotected bridge gives it an
    address the LAN can reach with nothing published. An external network's driver
    is not in the render, so it cannot be judged.
    """
    host, exposing, external = set(), set(), set()
    for key, net in (doc.get("networks") or {}).items():
        net = net or {}
        opts = net.get("driver_opts") or {}
        if key == "host" or net.get("name") == "host":
            host.add(key)
        elif net.get("external"):
            external.add(key)
        elif bridge_exposes(net.get("driver"), opts):
            exposing.add(key)
    return host, exposing, external


def rendered(doc):
    services = doc.get("services") if isinstance(doc, dict) else None
    if not isinstance(services, dict):
        raise Unjudgeable()
    host_nets, exposing_nets, external_nets = network_verdicts(doc)
    problems = []
    for service in sorted(services):
        spec = services[service]
        if not isinstance(spec, dict):
            raise Unjudgeable()
        mode = str(spec.get("network_mode") or "")
        if mode == "host":
            problems.append(f"posture: {service} network_mode=host refused")
            continue
        attached = set(spec.get("networks") or {})
        if host_nets & attached:
            problems.append(f"posture: {service} network=host refused")
            continue
        for net in sorted(exposing_nets & attached):
            problems.append(f"posture: {service} network={net} refused (reachable without a publication)")
        # `service:X` shares X's namespace, and X is judged on its own; `container:<id>`
        # names a namespace outside this render, as does an external network's driver.
        if mode.startswith("container:") or external_nets & attached:
            raise Unjudgeable()
        env = env_map(spec.get("environment"))
        for entry in spec.get("ports") or []:
            if not isinstance(entry, dict):
                raise Unjudgeable()
            proto = entry.get("protocol") or "tcp"
            host_ip = entry.get("host_ip") or ""
            judge(service, as_port(entry.get("target")), proto, host_ip, entry.get("published"), env, problems)
    return problems


def live(doc):
    # {"containers": <docker inspect>, "networks": <docker network inspect>} from concord-ctl;
    # a bare container list (no network data) is judged on bindings and modes only.
    networks = None
    if isinstance(doc, dict):
        networks = {n.get("Id"): n for n in doc.get("networks") or [] if isinstance(n, dict)}
        doc = doc.get("containers")
    if not isinstance(doc, list):
        raise Unjudgeable()
    # Compose turns `service:X` into `container:<X's id>`; X is inspected alongside.
    known = {c.get("Id") for c in doc if isinstance(c, dict)} \
        | {str(c.get("Name") or "").lstrip("/") for c in doc if isinstance(c, dict)}
    problems = []
    for container in doc:
        if not isinstance(container, dict):
            raise Unjudgeable()
        config = container.get("Config") or {}
        host = container.get("HostConfig") or {}
        service = (config.get("Labels") or {}).get("com.docker.compose.service")
        if not service:
            raise Unjudgeable()
        mode = str(host.get("NetworkMode") or "")
        if mode == "host":
            problems.append(f"posture: {service} network_mode=host refused")
            continue
        if mode.startswith("container:") and mode.partition(":")[2] not in known:
            raise Unjudgeable()  # the shared namespace belongs to a container outside this project
        if host.get("PublishAllPorts"):
            problems.append(f"posture: {service} publish_all refused")
            continue
        if networks is not None:
            attached = ((container.get("NetworkSettings") or {}).get("Networks") or {}).items()
            for name, endpoint in sorted(attached):
                net = networks.get((endpoint or {}).get("NetworkID"))
                if net is None:
                    raise Unjudgeable()  # attached to a network the caller did not inspect
                driver = net.get("Driver")
                if driver in ("null", "host"):
                    continue  # `none` has no path in; `host` mode is judged above
                if bridge_exposes(driver, net.get("Options")):
                    problems.append(f"posture: {service} network={name} refused (reachable without a publication)")
        env = env_map(config.get("Env"))
        for key, binds in sorted((host.get("PortBindings") or {}).items()):
            target_text, _, proto = str(key).partition("/")
            target = as_port(target_text)
            for bind in binds or []:
                if not isinstance(bind, dict):
                    raise Unjudgeable()
                judge(service, target, proto or "tcp", bind.get("HostIp") or "", bind.get("HostPort"), env, problems)
    return problems


def main(argv):
    if len(argv) != 2 or argv[1] not in ("--rendered", "--live"):
        print("usage: selfhost-port-posture.py --rendered|--live < json", file=sys.stderr)
        return 2
    sys.stdout.reconfigure(errors="backslashreplace")  # an unencodable name must not become a traceback
    try:
        doc = json.load(sys.stdin)
        problems = rendered(doc) if argv[1] == "--rendered" else live(doc)
    except Exception:  # fail closed WITHOUT echoing input: exception text can quote env values
        print("posture: input could not be judged (unparseable or unexpected shape)", file=sys.stderr)
        return 2
    for line in problems:
        print(line)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
