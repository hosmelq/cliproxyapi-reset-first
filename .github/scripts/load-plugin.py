"""Load a built plugin library the way CPA does and check what it registers."""

import ctypes
import json
import pathlib
import sys


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


Call = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(Buffer))
Free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)


class PluginAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("call", Call),
        ("free_buffer", Free),
        ("shutdown", ctypes.c_void_p),
    ]


path, version = sys.argv[1], sys.argv[2]

library = ctypes.CDLL(str(pathlib.Path(path).resolve()))
library.cliproxy_plugin_init.argtypes = [ctypes.c_void_p, ctypes.POINTER(PluginAPI)]

api = PluginAPI()
if library.cliproxy_plugin_init(None, ctypes.byref(api)) != 0:
    sys.exit("cliproxy_plugin_init failed")

response = Buffer()
status = api.call(b"plugin.register", None, 0, ctypes.byref(response))
raw = ctypes.string_at(response.ptr, response.len)
api.free_buffer(response.ptr, response.len)

envelope = json.loads(raw)
metadata = envelope.get("result", {}).get("metadata", {})
if status != 0 or not envelope.get("ok") or metadata.get("Version") != version:
    sys.exit(f"unexpected plugin.register response: {raw.decode()}")

print(f"{path} loaded and registered {metadata['Name']} {metadata['Version']}")
