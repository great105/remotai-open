"""Adapter checks against a frozen AgentDesk source; no model downloads."""
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from managed_bridge import ManagedBridge, BridgeError, CATALOG


class ManagementTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.bridge = ManagedBridge(Path(self.directory.name))

    def tearDown(self):
        self.bridge.close()
        self.directory.cleanup()

    def test_catalog_and_cpu_support_come_from_module(self):
        with patch("model_setup.detect_hardware", return_value=SimpleNamespace(name="CPU", has_nvidia=False, memory_mb=0)), patch("model_setup.cached_model", return_value=None), patch("model_setup.model_ready", return_value=False):
            result = self.bridge.dispatch("local.inspect", {})
        self.assertEqual({model["id"] for model in result["models"]}, set(CATALOG))
        self.assertTrue(all(model["supported"] == (not model["gpu_only"]) for model in result["models"]))

    def test_invalid_dictionary_preserves_all_preferences(self):
        before = [self.bridge.store.setting(key) for key in ("model", "language", "dictionary")]
        with patch("model_setup.model_ready", return_value=True):
            with self.assertRaises(BridgeError):
                self.bridge.dispatch("local.settings.save", {"model":"small", "language":"en", "dictionary":[{"heard":"x","written":""}]})
        self.assertEqual(before, [self.bridge.store.setting(key) for key in ("model", "language", "dictionary")])

    def test_preferences_are_saved_together_in_private_store(self):
        settings = {"model":"small","language":"en","dictionary":[{"heard":"ремотай","written":"Remotai"}]}
        with patch("model_setup.model_ready", return_value=True), patch.object(self.bridge, "inspect", return_value={}):
            self.bridge.dispatch("local.settings.save", settings)
        for key, value in settings.items(): self.assertEqual(self.bridge.store.setting(key), value)

    def test_install_delegates_to_original_installer(self):
        with patch("model_setup.fetch_model") as download, patch.object(self.bridge, "inspect", return_value={}):
            self.bridge.dispatch("local.model.install", {"model":"large-v3-turbo"})
            download.assert_called_once_with("large-v3-turbo")
        for invalid in ([], "unknown", None):
            with self.assertRaises(BridgeError): self.bridge.dispatch("local.model.install", {"model":invalid})

    def test_unready_model_cannot_be_selected(self):
        with patch("model_setup.model_ready", return_value=False):
            with self.assertRaises(BridgeError) as error:
                self.bridge.dispatch("local.settings.save", {"model":"small","language":"ru","dictionary":[]})
        self.assertEqual(error.exception.code, "model_not_ready")


if __name__ == "__main__": unittest.main()
