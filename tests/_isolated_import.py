"""Изолированный импорт модулей analytics_api в тестах.

analytics_api использует пакет верхнего уровня `src`. Чтобы загрузить модули analytics_api без правки sys.path для всех тестов,
временно убираем `src.*` из sys.modules, ставим каталог
analytics_api первым в sys.path, импортируем нужное и восстанавливаем исходное
состояние. Загруженные модули остаются рабочими: они держат ссылки на свои
зависимости, хотя в sys.modules их уже нет.
"""

import importlib
import sys
from pathlib import Path
from types import ModuleType
from typing import List

ANALYTICS_API_DIR = str(Path(__file__).resolve().parent.parent / "apps" / "analytics_api")


def _is_src(name: str) -> bool:
    return name == "src" or name.startswith("src.")


def load_analytics_modules(*module_names: str) -> List[ModuleType]:
    """Импортировать модули analytics_api (например, "src.main") в изоляции.

    Возвращает модули в том же порядке, что и имена.
    """
    saved_modules = {k: v for k, v in sys.modules.items() if _is_src(k)}
    saved_path = list(sys.path)
    for k in saved_modules:
        del sys.modules[k]
    sys.path.insert(0, ANALYTICS_API_DIR)
    importlib.invalidate_caches()
    try:
        return [importlib.import_module(name) for name in module_names]
    finally:
        for k in [k for k in sys.modules if _is_src(k)]:
            del sys.modules[k]
        sys.modules.update(saved_modules)
        sys.path[:] = saved_path
        importlib.invalidate_caches()
