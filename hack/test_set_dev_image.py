import pathlib
import subprocess
import unittest

SCRIPT = pathlib.Path(__file__).with_name("set-dev-image.py")
SHA = "a" * 40
IMAGE = f"ghcr.io/ictsc/ictsc-kocto:{SHA}"
BUILDER = f"ghcr.io/ictsc/ictsc-kocto-bundle-builder:{SHA}"
MANIFEST = """kind: Deployment
spec:
  template:
    spec:
      containers:
      - args:
        - --leader-elect
        - --health-probe-bind-address=:8081
        image: controller:latest
      serviceAccountName: ictsc-kocto-controller-manager
"""


class RenderTest(unittest.TestCase):
    def render(self, *args, manifest=MANIFEST):
        return subprocess.run(["python3", str(SCRIPT), IMAGE, *args],
                              input=manifest, text=True, capture_output=True)

    def test_disabled(self):
        result = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("--bundle-builder-image", result.stdout)
        self.assertNotIn("imagePullSecrets", result.stdout)

    def test_matching_release(self):
        result = self.render("ghcr-pull", BUILDER, "ghcr-pull")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"image: {IMAGE}", result.stdout)
        self.assertIn(f"--bundle-builder-image={BUILDER}", result.stdout)
        self.assertIn("--bundle-builder-pull-secret=ghcr-pull", result.stdout)
        self.assertIn("imagePullSecrets:", result.stdout)

    def test_digest(self):
        digest = "ghcr.io/ictsc/ictsc-kocto-bundle-builder@sha256:" + "b" * 64
        result = self.render("ghcr-pull", digest)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"--bundle-builder-image={digest}", result.stdout)

    def test_rejects_mismatched_or_invalid_inputs_without_output(self):
        for args in [("", BUILDER.replace(SHA, "other")),
                     ("", "example.invalid/builder:latest"),
                     ("", BUILDER, "invalid/secret"),
                     ("", "", "ghcr-pull")]:
            with self.subTest(args=args):
                result = self.render(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")

    def test_requires_unique_manager_anchor(self):
        for manifest in [MANIFEST.replace("- --health-probe-bind-address=:8081", "- --another-argument"),
                         MANIFEST + "- --health-probe-bind-address=:8081\n"]:
            result = self.render("ghcr-pull", BUILDER, manifest=manifest)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
