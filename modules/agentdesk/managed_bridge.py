"""Remotai management adapter. ASR, cache and installation belong to AgentDesk."""
from __future__ import annotations

import json
import sys
from pathlib import Path

import agent_bridge
from agent_bridge import AgentBridge, BridgeError
from model_catalog import CATALOG, serialized_catalog

MANAGEMENT_METHODS = ("local.inspect", "local.settings.save", "local.model.install")


class ManagedBridge(AgentBridge):
    def inspect(self):
        from model_setup import cached_model, detect_hardware, model_ready, recommended_model
        hardware = detect_hardware()
        models = []
        for item in serialized_catalog():
            item["label"] = item["label"].split(" · ")[0]
            item["cached"] = cached_model(item["id"]) is not None
            item["ready"] = model_ready(item["id"])
            item["supported"] = not item["gpu_only"] or hardware.has_nvidia
            models.append(item)
        version_file = Path(getattr(sys, "_MEIPASS", Path(__file__).parent)) / "module-version.json"
        version = json.loads(version_file.read_text(encoding="utf-8"))["version"] if version_file.is_file() else "remotai-local-dev"
        return {
            "management": True, "module_version": version, "models": models,
            "hardware": {"name": hardware.name, "has_nvidia": hardware.has_nvidia,
                         "memory_mb": hardware.memory_mb, "recommended_model": recommended_model(hardware)},
            "settings": {"model": self.default_model(), "language": self.store.setting("language", "ru"),
                         "dictionary": self.store.setting("dictionary", [])},
        }

    def dispatch(self, method, params):
        if not isinstance(params, dict):
            raise BridgeError("invalid_params", "params должен быть объектом")
        if method == "system.capabilities":
            result = super().dispatch(method, params)
            result["methods"] += list(MANAGEMENT_METHODS)
            result["management_version"] = 1
            return result
        if method == "local.inspect":
            return self.inspect()
        if method == "local.model.install":
            model = params.get("model")
            if not isinstance(model, str) or model not in CATALOG:
                raise BridgeError("invalid_params", "Неизвестная модель")
            from model_setup import fetch_model
            fetch_model(model)
            return self.inspect()
        if method == "local.settings.save":
            model, language, dictionary = params.get("model"), params.get("language"), params.get("dictionary")
            if not isinstance(model, str) or model not in CATALOG or language not in ("ru", "en", "auto"):
                raise BridgeError("invalid_params", "Выберите модель и язык")
            from model_setup import model_ready, detect_hardware
            if not model_ready(model):
                raise BridgeError("model_not_ready", "Сначала установите модель и её движок")
            if CATALOG[model].gpu_only and not detect_hardware().has_nvidia:
                raise BridgeError("unsupported_model", "Для этой модели нужна NVIDIA CUDA")
            if CATALOG[model].russian_only and language == "en":
                raise BridgeError("invalid_params", "Эта модель поддерживает только русский язык")
            if not isinstance(dictionary, list) or len(dictionary) > 100:
                raise BridgeError("invalid_params", "Словарь: не больше 100 замен")
            for row in dictionary:
                if not isinstance(row, dict) or any(not isinstance(row.get(key), str) or not 0 < len(row[key].strip()) <= 200 for key in ("heard", "written")):
                    raise BridgeError("invalid_params", "Каждая замена содержит исходный текст и результат")
            # Validate the whole request before saving any preference.
            with self.store.db:
                for key, value in (("model", model), ("language", language), ("dictionary", dictionary)):
                    self.store.db.execute("INSERT OR REPLACE INTO settings VALUES (?,?)", (key, json.dumps(value, ensure_ascii=False)))
            return self.inspect()
        return super().dispatch(method, params)


def main():
    # Reuse the original CLI, including DLL setup, path restrictions and cleanup.
    agent_bridge.AgentBridge = ManagedBridge
    from bridge_main import main as original_main
    return original_main()


if __name__ == "__main__":
    sys.exit(main())
