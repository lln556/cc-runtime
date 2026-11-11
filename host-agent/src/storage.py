"""Descriptor-relative access to the untrusted shared mailbox (macOS/Linux)."""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import stat
import uuid


class Mailbox:
    """Trust the configured root's parent, never follow links inside the mailbox."""

    MAX_JSON = 4 * 1024 * 1024
    DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC

    def __init__(self, root):
        self.root = Path(root).absolute()
        # Canonicalize trusted ancestors (e.g. macOS /var -> /private/var),
        # but leave the mailbox itself unresolved so a root symlink is refused.
        self.anchor = self.root.parent.resolve() / self.root.name

    def relative(self, path):
        path = Path(path)
        relative = path.relative_to(self.root) if path.is_absolute() else path
        if ".." in relative.parts:
            raise ValueError(f"Path escapes mailbox: {path}")
        return relative

    @contextmanager
    def directory(self, path, *, create=False):
        parts = self.relative(path).parts
        if create:
            self.anchor.parent.mkdir(parents=True, exist_ok=True)
            try:
                self.anchor.mkdir(mode=0o700)
            except FileExistsError:
                pass
        descriptor = os.open(self.anchor, self.DIRECTORY_FLAGS)
        try:
            for part in parts:
                if create:
                    try:
                        os.mkdir(part, mode=0o700, dir_fd=descriptor)
                    except FileExistsError:
                        pass
                child = os.open(part, self.DIRECTORY_FLAGS, dir_fd=descriptor)
                os.close(descriptor)
                descriptor = child
            yield descriptor
        finally:
            os.close(descriptor)

    def mkdir(self, path):
        with self.directory(path, create=True):
            pass

    def atomic(self, path, value):
        path = self.relative(path)
        with self.directory(path.parent, create=True) as directory:
            temporary = ".tmp-" + uuid.uuid4().hex
            descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL |
                                 os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=directory)
            try:
                with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                    json.dump(value, stream, ensure_ascii=False, indent=2)
                    stream.write("\n")
                    stream.flush()
                    os.fsync(stream.fileno())
                os.replace(temporary, path.name, src_dir_fd=directory, dst_dir_fd=directory)
            finally:
                try:
                    os.unlink(temporary, dir_fd=directory)
                except FileNotFoundError:
                    pass

    @staticmethod
    def check_file(descriptor):
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ValueError("Mailbox files must be regular files with one link")
        return info

    @contextmanager
    def open_file(self, path, flags):
        path = self.relative(path)
        with self.directory(path.parent) as directory:
            descriptor = os.open(path.name, flags | os.O_NOFOLLOW | os.O_NONBLOCK |
                                 os.O_CLOEXEC, 0o600, dir_fd=directory)
        try:
            self.check_file(descriptor)
            yield descriptor
        finally:
            os.close(descriptor)

    def read(self, path):
        with self.open_file(path, os.O_RDONLY) as descriptor:
            if self.check_file(descriptor).st_size > self.MAX_JSON:
                raise ValueError(f"JSON file too large: {path}")
            with os.fdopen(os.dup(descriptor), "rb") as stream:
                data = stream.read(self.MAX_JSON + 1)
            if len(data) > self.MAX_JSON:
                raise ValueError(f"JSON file too large: {path}")
        value = json.loads(data)
        if not isinstance(value, dict):
            raise ValueError(f"Expected a JSON object: {path}")
        return value

    def exists(self, path):
        path = self.relative(path)
        try:
            with self.directory(path.parent) as directory:
                os.stat(path.name, dir_fd=directory, follow_symlinks=False)
            return True
        except FileNotFoundError:
            return False

    def json_files(self, path):
        path = self.relative(path)
        try:
            with self.directory(path) as directory:
                entries = []
                for name in os.listdir(directory):
                    if name.endswith(".json"):
                        try:
                            info = os.stat(name, dir_fd=directory, follow_symlinks=False)
                        except FileNotFoundError:
                            continue
                        entries.append((info.st_mtime_ns, name))
        except FileNotFoundError:
            return []
        return [self.root / path / name for _, name in sorted(entries)]

    def invalidate(self, path):
        path = self.relative(path)
        with self.directory(path.parent) as directory:
            try:
                os.replace(path.name, path.with_suffix(".invalid").name,
                           src_dir_fd=directory, dst_dir_fd=directory)
            except FileNotFoundError:
                pass

    def unlink(self, path):
        path = self.relative(path)
        with self.directory(path.parent) as directory:
            try:
                os.unlink(path.name, dir_fd=directory)
            except FileNotFoundError:
                pass

    @contextmanager
    def event_log(self, path):
        # Keep the descriptor for the whole turn: replacing its pathname later
        # cannot redirect subsequent events into another host file.
        with self.open_file(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT) as descriptor:
            with os.fdopen(os.dup(descriptor), "a", encoding="utf-8") as stream:
                yield stream

    def append_event(self, stream, event):
        self.check_file(stream.fileno())
        stream.write(json.dumps(event, ensure_ascii=False) + "\n")
        stream.flush()
