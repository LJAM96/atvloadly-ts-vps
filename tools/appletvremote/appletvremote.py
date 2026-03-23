#!/opt/pmd3/bin/python

import argparse
import asyncio
import json
import sys
from contextlib import suppress
from pathlib import Path
from typing import Iterable

from pymobiledevice3.exceptions import ConnectionTerminatedError, RemotePairingCompletedError
from pymobiledevice3.remote.remote_service_discovery import RemoteServiceDiscoveryService
from pymobiledevice3.remote.tunnel_service import (
    RemotePairingManualPairingService,
    create_core_device_tunnel_service_using_remotepairing,
)
from pymobiledevice3.services.installation_proxy import InstallationProxyService

DIRECT_DEVICE_PREFIX = "ATVLOADLY_DIRECT_DEVICE="
PAIRING_PORT_RANGE = range(49152, 65536)
PROBE_TIMEOUT_SECONDS = 0.15
PROBE_BATCH_SIZE = 256


class AtvloadlyManualPairingService(RemotePairingManualPairingService):
    @property
    def remote_identifier(self) -> str:
        handshake_info = getattr(self, "handshake_info", None) or {}
        peer_info = handshake_info.get("peerDeviceInfo", {})
        identifier = peer_info.get("identifier")
        if identifier:
            return identifier
        return super().remote_identifier


def emit_direct_device(device: dict) -> None:
    print(f"{DIRECT_DEVICE_PREFIX}{json.dumps(device, separators=(',', ':'))}", flush=True)


async def is_port_open(host: str, port: int) -> int | None:
    try:
        reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout=PROBE_TIMEOUT_SECONDS)
    except Exception:
        return None

    writer.close()
    with suppress(Exception):
        await writer.wait_closed()
    return port


async def find_open_ports(host: str, ports: Iterable[int]) -> list[int]:
    open_ports: list[int] = []
    current_batch: list[int] = []

    for port in ports:
        current_batch.append(port)
        if len(current_batch) >= PROBE_BATCH_SIZE:
            results = await asyncio.gather(*(is_port_open(host, value) for value in current_batch))
            open_ports.extend([value for value in results if value is not None])
            current_batch = []

    if current_batch:
        results = await asyncio.gather(*(is_port_open(host, value) for value in current_batch))
        open_ports.extend([value for value in results if value is not None])

    return open_ports


def ordered_candidates(preferred_port: int | None, discovered_ports: list[int], exclude: set[int] | None = None) -> list[int]:
    exclude = exclude or set()
    ordered: list[int] = []

    if preferred_port and preferred_port not in exclude:
        ordered.append(preferred_port)

    for port in discovered_ports:
        if port in exclude or port in ordered:
            continue
        ordered.append(port)

    return ordered


async def get_lockdown_device_name(rsd: RemoteServiceDiscoveryService) -> str | None:
    if rsd.lockdown is None:
        return None

    try:
        return await rsd.get_value(key="DeviceName")
    except Exception:
        return None


async def probe_direct_device(host: str, identifier: str, tunnel_port: int | None = None, exclude: set[int] | None = None) -> dict:
    exclude = exclude or set()

    async def attempt_ports(ports: list[int]) -> dict | None:
        for port in ports:
            service = None
            rsd = None
            try:
                service = await create_core_device_tunnel_service_using_remotepairing(identifier, host, port)
                async with service.start_tcp_tunnel() as result:
                    rsd = RemoteServiceDiscoveryService((result.address, result.port))
                    await rsd.connect()
                    properties = rsd.peer_info["Properties"]
                    device_name = await get_lockdown_device_name(rsd)
                    return {
                        "host": host,
                        "name": device_name or properties.get("ProductName") or properties.get("ProductType") or "Apple TV",
                        "udid": properties["UniqueDeviceID"],
                        "remote_identifier": identifier,
                        "tunnel_port": port,
                        "device_class": properties.get("DeviceClass") or "AppleTV",
                        "product_type": properties.get("ProductType") or "",
                        "product_version": properties.get("OSVersion") or "",
                    }
            except Exception:
                continue
            finally:
                if rsd is not None:
                    with suppress(Exception):
                        await rsd.close()
                if service is not None:
                    with suppress(Exception):
                        await service.close()
        return None

    device = await attempt_ports(ordered_candidates(tunnel_port, [], exclude=exclude))
    if device is not None:
        return device

    discovered_ports = await find_open_ports(host, PAIRING_PORT_RANGE)
    candidates = ordered_candidates(tunnel_port, discovered_ports, exclude=exclude)
    if not candidates:
        raise RuntimeError(f"no open candidate ports found for {host}")

    device = await attempt_ports(candidates)
    if device is not None:
        return device

    raise RuntimeError(f"could not find a usable remote tunnel service for {host}")


async def find_manual_pair_identifier(host: str, pair_port: int | None = None) -> tuple[str, int] | None:
    async def attempt_ports(ports: list[int]) -> tuple[str, int] | None:
        for port in ports:
            service = AtvloadlyManualPairingService("probe", host, port)
            try:
                await service.connect(autopair=True)
                return service.remote_identifier, port
            except RemotePairingCompletedError:
                return service.remote_identifier, port
            except (ConnectionResetError, ConnectionTerminatedError, asyncio.IncompleteReadError, asyncio.TimeoutError, OSError):
                continue
            except Exception:
                continue
            finally:
                with suppress(Exception):
                    await service.close()
        return None

    paired = await attempt_ports(ordered_candidates(pair_port, []))
    if paired is not None:
        return paired

    discovered_ports = await find_open_ports(host, PAIRING_PORT_RANGE)
    candidates = ordered_candidates(pair_port, discovered_ports)
    if not candidates:
        return None

    return await attempt_ports(candidates)


async def pair(host: str, pair_port: int | None = None, tunnel_port: int | None = None) -> int:
    paired = await find_manual_pair_identifier(host, pair_port=pair_port)
    if paired is None:
        print(f"ERROR: failed to find a manual pairing service for {host}", flush=True)
        return 1

    paired_identifier, paired_port = paired

    device = await probe_direct_device(host, paired_identifier, tunnel_port=tunnel_port, exclude={paired_port})
    device["manual_pair_port"] = paired_port
    emit_direct_device(device)
    print("SUCCESS", flush=True)
    return 0


async def probe(host: str, identifier: str, tunnel_port: int | None = None) -> int:
    device = await probe_direct_device(host, identifier, tunnel_port=tunnel_port)
    emit_direct_device(device)
    print(json.dumps(device, indent=2), flush=True)
    return 0


async def install(host: str, identifier: str, package: str, tunnel_port: int | None = None) -> int:
    device = await probe_direct_device(host, identifier, tunnel_port=tunnel_port)
    emit_direct_device(device)

    service = await create_core_device_tunnel_service_using_remotepairing(identifier, host, device["tunnel_port"])
    async with service.start_tcp_tunnel() as result:
        rsd = RemoteServiceDiscoveryService((result.address, result.port))
        await rsd.connect()
        try:
            installation_proxy = InstallationProxyService(lockdown=rsd)

            def progress_handler(*args) -> None:
                progress = None
                if len(args) == 1 and isinstance(args[0], int):
                    progress = args[0]
                elif len(args) >= 2 and isinstance(args[1], int):
                    progress = args[1]
                elif len(args) > 0 and isinstance(args[0], dict):
                    value = args[0].get("PercentComplete") or args[0].get("percentComplete")
                    if isinstance(value, int):
                        progress = value

                if progress is not None:
                    print(f"{progress}% Complete", flush=True)

            await installation_proxy.install_from_local(Path(package), handler=progress_handler)
        finally:
            with suppress(Exception):
                await rsd.close()
            with suppress(Exception):
                await service.close()

    print("Installation complete", flush=True)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Apple TV direct-connect helper for atvloadly")
    subparsers = parser.add_subparsers(dest="command", required=True)

    pair_parser = subparsers.add_parser("pair", help="pair to an Apple TV by host")
    pair_parser.add_argument("--host", required=True)
    pair_parser.add_argument("--pair-port", type=int)
    pair_parser.add_argument("--tunnel-port", type=int)

    probe_parser = subparsers.add_parser("probe", help="resolve device metadata by host")
    probe_parser.add_argument("--host", required=True)
    probe_parser.add_argument("--identifier", required=True)
    probe_parser.add_argument("--tunnel-port", type=int)

    install_parser = subparsers.add_parser("install", help="install a signed IPA through a remote tunnel")
    install_parser.add_argument("--host", required=True)
    install_parser.add_argument("--identifier", required=True)
    install_parser.add_argument("--package", required=True)
    install_parser.add_argument("--tunnel-port", type=int)

    return parser


async def run(args: argparse.Namespace) -> int:
    if args.command == "pair":
        return await pair(args.host, pair_port=args.pair_port, tunnel_port=args.tunnel_port)
    if args.command == "probe":
        return await probe(args.host, args.identifier, tunnel_port=args.tunnel_port)
    if args.command == "install":
        return await install(args.host, args.identifier, args.package, tunnel_port=args.tunnel_port)

    raise RuntimeError(f"unsupported command: {args.command}")


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    try:
        return asyncio.run(run(args))
    except KeyboardInterrupt:
        return 130
    except Exception as exc:
        print(f"ERROR: {exc}", flush=True)
        return 1


if __name__ == "__main__":
    sys.exit(main())
