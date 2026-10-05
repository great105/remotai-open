"""Validate native CI artifacts before the release publisher can upload them."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--input', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    p.add_argument('--version', required=True)
    p.add_argument('--source', required=True)
    a = p.parse_args()
    expected = {
        'desktop-ubuntu-24.04': ['remotai-linux-amd64.deb', 'remotai-linux-amd64.rpm'],
        'desktop-ubuntu-24.04-arm': ['remotai-linux-arm64.deb', 'remotai-linux-arm64.rpm'],
        'desktop-macos-15': ['remotai-macos.dmg'],
    }
    packages = []
    proofs = {}
    for artifact, names in expected.items():
        directory = a.input / artifact
        proof = json.loads((directory / 'packages.json').read_text())
        assert proof['source'] == a.source and proof['version'] == a.version, 'Packages must match the release source and version'
        assert sorted(item['filename'] for item in proof['artifacts']) == sorted(names)
        proofs[artifact] = proof
        for item in proof['artifacts']:
            file = directory / item['filename']
            with file.open('rb') as stream:
                sha = hashlib.file_digest(stream, 'sha256').hexdigest()
            assert sha == item['sha256'] and file.stat().st_size == item['size'], 'Damaged package'
            packages.append((file, item))
    for runner, package_artifact, system, arch in [
        ('ubuntu-24.04', 'desktop-ubuntu-24.04', 'Linux', 'x86_64'),
        ('ubuntu-24.04-arm', 'desktop-ubuntu-24.04-arm', 'Linux', 'aarch64'),
        ('macos-15', 'desktop-macos-15', 'Darwin', 'arm64'),
        ('macos-15-intel', 'desktop-macos-15', 'Darwin', 'x86_64'),
    ]:
        acceptance = json.loads((a.input / ('desktop-acceptance-' + runner) / 'acceptance.json').read_text())
        assert acceptance['source'] == a.source and acceptance['version'] == a.version
        assert acceptance['platform'] == system and acceptance['machine'] == arch
        assert acceptance['artifacts'] == proofs[package_artifact]['artifacts'], 'Native acceptance must cover the exact distributed bytes'
        assert len(acceptance['checks']) >= (3 if system == 'Darwin' else 6)
        if system == 'Darwin':
            assert proofs[package_artifact].get('native_window') is True
            native = json.loads((a.input / ('desktop-acceptance-' + runner) / 'native-window.json').read_text())
            assert native['native_window'] and native['webview_loaded'] and native['dom_ready'] and native['dock_app'] and native['reopened']
            client = json.loads((a.input / ('desktop-acceptance-' + runner) / 'native-client.json').read_text())
            assert client['shared_client'] and client['dom_ready'] and client['native_window'] and client['reopened']
            assert client['agent_recovered'] and client['bootstrap_count'] == 2
    # No output is written until all three builds and four native runs agree.
    a.out.mkdir(parents=True, exist_ok=True)
    for file, item in packages:
        shutil.copyfile(file, a.out / item['filename'])
    mac = proofs['desktop-macos-15']
    manifest = {'version': a.version, 'source': a.source,
                'macos_developer_id_signed': mac['developer_id_signed'], 'macos_notarized': mac['notarized'],
                'artifacts': [item for _, item in packages]}
    (a.out / 'desktop-packages.json').write_text(json.dumps(manifest, indent=2))
    print(json.dumps(manifest, indent=2))


if __name__ == '__main__':
    main()
