from app.services import updates


class Completed:
    returncode = 0
    stdout = "0123456789abcdef0123456789abcdef01234567\trefs/heads/main\n"
    stderr = ""


def test_panel_status_checks_main_branch(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(updates, "UPDATE_STATE_FILE", tmp_path / "update-status.json")
    monkeypatch.setattr(updates, "UPDATE_BRANCH", "main")

    def fake_run(argv, **kwargs):
        calls.append((argv, kwargs))
        return Completed()

    monkeypatch.setattr(updates.subprocess, "run", fake_run)

    result = updates.panel_release_status(force_refresh=True)

    assert result["update_channel"] == "branch"
    assert result["update_branch"] == "main"
    assert result["latest_tag"] == "origin/main"
    assert result["latest_commit"] == "0123456789abcdef0123456789abcdef01234567"
    assert result["update_available"] is None
    assert calls[0][0][:3] == ["git", "ls-remote", "--heads"]
    assert calls[0][0][-1] == "refs/heads/main"
    assert "--tags" not in calls[0][0]
