import tempfile
import unittest
from pathlib import Path

import runner_policy


class RunnerPolicyTests(unittest.TestCase):
    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text(
                "julianknutsen\n"
                "  Csells  # maintainer\n"
                "\n"
                "# comment\n",
                encoding="utf-8",
            )

            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_pull_request_from_allowlisted_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "Quad341",
            {"quad341"},
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("allowlist", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_push_uses_github_even_for_allowlisted_author(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("approved pull requests", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")

    def test_forced_workflow_call_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_call",
            "",
            set(),
            force_blacksmith=True,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("forced", reason)
        self.assertEqual(runners["runner_16vcpu"], "blacksmith-16vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_unlisted_pull_request_author_uses_github(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("not on the Blacksmith allowlist", reason)
        self.assertEqual(runners["runner_macos"], "macos-15")

    def test_our_fork_uses_self_hosted_for_every_event(self) -> None:
        for event_name in (
            "pull_request",
            "push",
            "schedule",
            "workflow_dispatch",
            "workflow_call",
        ):
            with self.subTest(event=event_name):
                use_blacksmith, reason, runners = runner_policy.select_runners(
                    event_name,
                    "external-contributor",
                    set(),
                    repository="material-lab-io/gascity",
                )

                self.assertFalse(use_blacksmith)
                self.assertEqual(runners, runner_policy.SELF_HOSTED_RUNNERS)
                self.assertEqual(runners["runner_2vcpu"], "gt2")
                self.assertEqual(runners["runner_32vcpu"], "gt2")
                self.assertEqual(runners["runner_macos"], "macos")
                self.assertIn("material-lab-io/gascity", reason)

    def test_our_fork_stays_self_hosted_even_with_force_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_dispatch",
            "",
            set(),
            force_blacksmith=True,
            repository="material-lab-io/gascity",
        )

        self.assertFalse(use_blacksmith)
        self.assertEqual(runners, runner_policy.SELF_HOSTED_RUNNERS)

    def test_our_fork_stays_self_hosted_even_on_the_blacksmith_allowlist(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "julianknutsen",
            {"julianknutsen"},
            repository="material-lab-io/gascity",
        )

        self.assertFalse(use_blacksmith)
        self.assertEqual(runners, runner_policy.SELF_HOSTED_RUNNERS)

    def test_other_repositories_are_unaffected(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "",
            set(),
            repository="gastownhall/gascity",
        )

        self.assertFalse(use_blacksmith)
        self.assertEqual(runners, runner_policy.GITHUB_RUNNERS)


if __name__ == "__main__":
    unittest.main()
