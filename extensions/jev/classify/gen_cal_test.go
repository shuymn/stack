package classify

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/shuymn/stack/extensions/internal/jev"
)

// This file writes fixtures/jev/classify/cal: the synthetic calibration corpus (§3.5), drafted
// from the quotas and the label definitions, never from a real reply, path or name. Every case is
// written pending: its label and `why` are hand-written here and checked against the label
// definitions by a reviewer model before any score is seen, and freeze then; a held-out label
// changed after scores were seen means replacing that case with a fresh one. Run it with
// JEV_GEN_FIXTURES=1.

// calCase is one corpus item. dup is "duplicate" or "new" for a correcting item.
type calCase struct {
	id, lang, kind, dup, group string
	overlap, hard              bool
	dir                        string
	task, closing, reply, why  string
}

const (
	dupY = DupDuplicate
	dupN = DupNew
	cor  = LabelCorrection
	dri  = LabelDrift
	app  = LabelApproval
	req  = LabelRequest
	noi  = LabelNoise
)

type calFixture struct {
	name, split string
	sources     map[string]bool // dir → T3
	index       map[string]string
	cases       []calCase
}

//nolint:dupl,gosmopolitan // two synthetic weeks of the same shape, in two languages
var calFixtures = []calFixture{
	{
		name: "fit-a", split: "fit",
		sources: map[string]bool{"p1": false, "p2": true, "p3": false},
		index: map[string]string{
			"p1": "- Always run scripts/test before pushing.\n- Never edit files under vendor/.\n" +
				"- Commit messages follow Conventional Commits.\n- 返信は日本語で書く。\n" +
				"- Keep each PR under 400 changed lines.\n- Do not add new dependencies without asking.\n" +
				"- Never change money rounding rules without asking.\n",
			"p2": "- Use sentence case for headings.\n- Screenshots go in docs/img and need alt text.\n" +
				"- Never rewrite the changelog history.\n- Run the link checker before publishing.\n",
			"p3": "- 依頼された範囲だけを変更する。ついでの書き換えはしない。\n" +
				"- Keep components small.\n- Use the shared date formatter.\n",
		},
		cases: []calCase{
			{
				"fit-a01",
				"en",
				dri,
				dupY,
				"pair-1",
				false,
				true,
				"p1",
				"Do not change any rounding behaviour; I only want the log format fixed.",
				"I changed the invoice total rounding from half-up to banker's rounding in billing/total.go.",
				"revert that",
				"restores the owner's scope (log format only) after the agent changed rounding; an index line forbids rounding changes",
			},
			{
				"fit-a02",
				"en",
				req,
				"",
				"pair-1",
				false,
				true,
				"p1",
				"Please switch invoice totals to banker's rounding.",
				"I changed the invoice total rounding from half-up to banker's rounding in billing/total.go.",
				"revert that",
				"the same assistant and reply as fit-a01, but the owner had asked for the rounding change, so the reply is a new task",
			},
			{
				"fit-a03",
				"ja",
				dri,
				dupN,
				"pair-2",
				false,
				true,
				"p2",
				"見出しのスタイルは変更しないで、リンク切れだけ直して。",
				"docs/guide.md の見出しをすべて Title Case に書き換えました。",
				"戻して",
				"restores the scope (links only) after a heading rewrite; the index states a heading style rule, not this scope",
			},
			{
				"fit-a04",
				"ja",
				req,
				"",
				"pair-2",
				false,
				true,
				"p2",
				"見出しを Title Case に統一して。",
				"docs/guide.md の見出しをすべて Title Case に書き換えました。",
				"戻して",
				"the same assistant and reply as fit-a03, but the owner had asked for the rewrite, so the reply is a new task",
			},
			{
				"fit-a05", "en", cor, dupY, "", false, false, "p1",
				"Fix the date parsing bug in the importer.",
				"I pushed the branch after the unit tests passed.",
				"no, you have to run scripts/test, not just the unit tests",
				"says the agent's reading of the check was wrong; the index states the rule",
			},
			{
				"fit-a06", "ja", cor, dupY, "", false, true, "p1",
				"ログ出力のバグを直して。",
				"vendor/ 内のライブラリのバグを直接修正しました。",
				"vendor は触らないでって言ったよね？",
				"a correction asked as a question; the index states the vendor rule",
			},
			{
				"fit-a07", "en", cor, dupN, "", false, true, "p1",
				"Add tests for the invoice exporter.",
				"I added three tests, each a separate function with copied setup.",
				"write tests in table-driven style please",
				"a standing rule that never says wrong; no index line covers test style",
			},
			{
				"fit-a08",
				"ja",
				cor,
				dupN,
				"",
				false,
				true,
				"p1",
				"リリースノートの草稿をコミットして。",
				"コミットメッセージに絵文字を付けました。",
				"絵文字は使わないで",
				"a correction; the index only states the Conventional Commits format, a different rule on the same topic",
			},
			{
				"fit-a09", "en", cor, dupY, "", false, false, "p1",
				"Split the exporter into its own package.",
				"I opened a PR with 900 changed lines.",
				"that PR is way too big, split it",
				"says the approach was wrong; the index states the size limit",
			},
			{
				"fit-a10",
				"en",
				dri,
				dupN,
				"",
				false,
				false,
				"p1",
				"Fix the null pointer in the invoice exporter only.",
				"I fixed it, and I also refactored the whole exporter package and renamed three types.",
				"I asked for the null pointer fix only; undo the refactor",
				"restores the scope the owner set; no index line states a scope rule",
			},
			{
				"fit-a11", "ja", dri, dupY, "", false, false, "p3",
				"管理画面のボタン文言だけ直して。",
				"文言のついでにレイアウトも書き換えました。",
				"文言だけって言ったでしょ、レイアウトは戻して",
				"restores the scope the owner set; the index line states the same scope rule",
			},
			{
				"fit-a12",
				"en",
				dri,
				dupN,
				"",
				false,
				true,
				"p1",
				"Add a retry to the webhook sender.",
				"I added retries and also changed the timeout defaults for every client.",
				"just the webhook retry, put the timeouts back",
				"restores the scope after the agent changed shared defaults; no index line covers scope",
			},
			{
				"fit-a13",
				"en",
				dri,
				dupY,
				"",
				true,
				false,
				"p2",
				"Update the README install steps.",
				"I rewrote the install steps and deleted the changelog section as outdated.",
				"never delete changelog entries, and only touch the install steps",
				"an overlap: corrects the deletion and restores the scope; the index states the changelog rule",
			},
			{
				"fit-a14",
				"ja",
				dri,
				dupN,
				"",
				true,
				true,
				"p1",
				"ログイン API のエラーメッセージだけ直して。",
				"メッセージを直して、ステータスコードも 200 に変えました。",
				"ステータスコードは触らないで。それは間違い、メッセージだけ",
				"an overlap: says the change was wrong and restores the scope; no index line covers it",
			},
			{
				"fit-a15",
				"ja",
				dri,
				dupY,
				"",
				true,
				true,
				"p1",
				"CI の Go のバージョンだけ上げて。",
				"Go を上げて、ついでに vendor/ の依存を 3 つ更新しました。",
				"それは違う。vendor は触らないで、Go のバージョンだけ",
				"an overlap: corrects the vendor edit and restores the scope; the index states the vendor rule",
			},
			{
				"fit-a16", "en", app, "", "", false, false, "p1",
				"Run the migration on staging.", "Migration finished; all checks are green.",
				"ok, merged", "acknowledges a result",
			},
			{
				"fit-a17", "ja", app, "", "", false, false, "p2",
				"リンクチェッカーを実行して。", "チェックが完了し、問題はありませんでした。",
				"OK、マージしました", "acknowledges a result",
			},
			{
				"fit-a18",
				"en",
				app,
				"",
				"",
				false,
				true,
				"p1",
				"Use the cached client in the exporter.",
				"I switched to the cached client as discussed.",
				"no, that's fine — ship it",
				"opens with no but accepts the change",
			},
			{
				"fit-a19",
				"en",
				req,
				"",
				"",
				false,
				true,
				"p1",
				"Add an export for empty invoices.",
				"The export for empty invoices is in place.",
				"the export still crashes on empty invoices",
				"reports a defect in what was built, without saying the agent worked wrongly",
			},
			{
				"fit-a20", "ja", noi, "", "", false, true, "p2",
				"画像の説明を確認して。", "画像の説明を確認しました。",
				"テスト、てすと", "an input-method test sentence that reads as text",
			},
			{
				"fit-a21", "en", noi, "", "", false, false, "p1",
				"Show the failing commit.", "The failing commit is the one before the rebase.",
				"3f9c2ab", "a lone hash",
			},
			{
				"fit-a22", "ja", noi, "", "", false, false, "p2",
				"PR のリンクをください。", "PR を作成しました。",
				"https://example.com/pr/42", "a lone link",
			},
		},
	},
	{
		name: "fit-b", split: "fit",
		sources: map[string]bool{"p1": true, "p2": false, "p3": true},
		index: map[string]string{
			"p1": "- Prefer small, composable functions; no function over 60 lines.\n" +
				"- All user-facing strings go through the i18n helper.\n" +
				"- Write tests as table-driven tests.\n- Exit codes: 0 ok, 1 error, 2 usage.\n" +
				"- 出力は常に UTF-8 で扱う。\n",
			"p2": "- Never delete raw input files after a run.\n- Schedules are defined in pipeline.toml, not in code.\n" +
				"- Keep SQL in .sql files, not in Go strings.\n- Backfills need the --dry-run pass first.\n" +
				"- ログは構造化 JSON で出す。\n",
			"p3": "- Stay within the requested change; no rewrites.\n- Keep the README in sync with the flags.\n" +
				"- Run gofmt before committing.\n",
		},
		cases: []calCase{
			{
				"fit-b01",
				"en",
				dri,
				dupN,
				"pair-3",
				false,
				true,
				"p1",
				"Only add the --json flag; leave the rest of the flag parser alone.",
				"I rewrote the flag parser to use a new library; every existing flag still works.",
				"revert the parser",
				"restores the scope (one flag) after a parser rewrite; no index line covers scope in this directory",
			},
			{
				"fit-b02",
				"en",
				req,
				"",
				"pair-3",
				false,
				true,
				"p1",
				"Replace the flag parser with the new library.",
				"I rewrote the flag parser to use a new library; every existing flag still works.",
				"revert the parser",
				"the same assistant and reply as fit-b01, but the owner had asked for the replacement",
			},
			{
				"fit-b03", "ja", dri, dupY, "pair-4", false, true, "p2",
				"バックフィルはまず --dry-run だけ実行して結果を見せて。",
				"バックフィルを本番データに対して直接実行しました。",
				"止めて、元に戻して",
				"restores the scope (dry run only); the index states the dry-run rule",
			},
			{
				"fit-b04", "ja", req, "", "pair-4", false, true, "p2",
				"バックフィルを本番に流して。",
				"バックフィルを本番データに対して直接実行しました。",
				"止めて、元に戻して",
				"the same assistant and reply as fit-b03, but the owner had asked for the run",
			},
			{
				"fit-b05", "en", cor, dupY, "", false, false, "p2",
				"Add the monthly rollup query.",
				"I put the query inline in a Go string in rollup.go.",
				"SQL belongs in .sql files",
				"says the approach was wrong; the index states the rule",
			},
			{
				"fit-b06", "ja", cor, dupY, "", false, false, "p2",
				"取り込み処理のディスク使用量を減らして。",
				"取り込み後に生データのファイルを削除する処理を追加しました。",
				"生データは消しちゃだめ。",
				"says the approach was wrong; the index states the rule",
			},
			{
				"fit-b07", "en", cor, dupN, "", false, true, "p2",
				"Make the fetcher more robust.",
				"I hard-coded the retry count to 5.",
				"isn't that a setting?",
				"a correction asked as a question; no index line covers settings",
			},
			{
				"fit-b08", "ja", cor, dupN, "", false, false, "p2",
				"CSV の読み込みを速くして。",
				"新しい依存ライブラリを 3 つ追加しました。",
				"依存は増やさない方針です",
				"states a standing rule; no index line in this directory covers dependencies",
			},
			{
				"fit-b09", "en", cor, dupN, "", false, true, "p1",
				"Add a helper that formats durations.",
				"I named the function DoStuff2.",
				"naming: verbs plus the object, please",
				"a standing rule that never says wrong",
			},
			{
				"fit-b10", "en", dri, dupY, "", false, false, "p3",
				"Fix the off-by-one in the pager.",
				"I fixed it and rewrote the pagination to use cursors.",
				"no cursors, I only wanted the off-by-one fix",
				"restores the scope the owner set; the index states the scope rule",
			},
			{
				"fit-b11", "ja", dri, dupN, "", false, false, "p1",
				"ヘルプ文言の誤字だけ直して。",
				"誤字に加えて、コマンド名も変更しました。",
				"コマンド名は戻して。誤字だけ。",
				"restores the scope the owner set; no index line covers scope",
			},
			{
				"fit-b12", "ja", dri, dupN, "", false, true, "p2",
				"取り込み処理に処理行数のログを足して。",
				"行数のログを足して、ロガーのライブラリも切り替えました。",
				"今のロガーのままで。行数のログだけ",
				"restores the scope; the index only covers the log format, not the logger choice",
			},
			{
				"fit-b13",
				"ja",
				dri,
				dupY,
				"",
				true,
				true,
				"p1",
				"run コマンドを exec に改名して。",
				"改名に加えて、終了コードを 0/1/3 に変えました。",
				"exit code は 0/1/2 のまま。改名だけって言ったでしょ",
				"an overlap: corrects the exit codes and restores the scope; the index states the exit codes",
			},
			{
				"fit-b14",
				"ja",
				dri,
				dupY,
				"",
				true,
				false,
				"p2",
				"ログの出力先だけ変えて。",
				"ログをプレーンテキストに変えました。",
				"ログは JSON のまま。出力先だけって言ったでしょ",
				"an overlap: corrects the format change and restores the scope; the index states the log format",
			},
			{
				"fit-b15",
				"en",
				cor,
				dupN,
				"",
				false,
				true,
				"p1",
				"Add a --quiet flag.",
				"I added --quiet and put the verbosity level into a package-level variable.",
				"no globals in handlers, pass the level in",
				"a standing rule about the implementation, never saying wrong; no index line covers globals",
			},
			{
				"fit-b16", "ja", app, "", "", false, false, "p2",
				"集計クエリを直して。", "集計クエリを修正し、テストも通りました。",
				"いいね、マージして", "accepts the change",
			},
			{
				"fit-b17", "en", app, "", "", false, false, "p1",
				"Show me option A and B.", "Option B keeps the old flags; option A drops them.",
				"yes, that's what I wanted", "accepts the result",
			},
			{
				"fit-b18", "ja", app, "", "", false, false, "p2",
				"スケジュールを確認して。", "pipeline.toml の設定を確認しました。",
				"確認しました、OK", "acknowledges a result",
			},
			{
				"fit-b19",
				"ja",
				req,
				"",
				"",
				false,
				true,
				"p1",
				"フラグの説明を更新して。",
				"README を更新しました。",
				"CI が落ちてるけど原因わかる？",
				"reports a status and asks a question, without saying the agent worked wrongly",
			},
			{
				"fit-b20", "en", noi, "", "", false, true, "p1",
				"Check the build output.", "The build output looks fine.",
				"asdf test", "an input test that reads as text",
			},
			{
				"fit-b21", "ja", noi, "", "", false, true, "p2",
				"ログを見せて。", "直近のログを貼りました。",
				"スクリーンショット添付", "a bare attachment note",
			},
			{
				"fit-b22", "en", noi, "", "", false, false, "p1",
				"Where is the failing log?", "The log is in the CI artifacts.",
				"https://example.com/log/881", "a lone link",
			},
		},
	},
	{
		name: "heldout-a", split: "heldout",
		sources: map[string]bool{"p1": false, "p2": true, "p3": false},
		index: map[string]string{
			"p1": "- Never commit signing keys or keystores.\n- Use the design tokens; no hard-coded colours.\n" +
				"- Every screen needs an accessibility label.\n- Release builds are cut from release/* branches only.\n" +
				"- 新しい画面には必ずプレビューを付ける。\n",
			"p2": "- Terraform changes go through plan review; never apply from a laptop.\n" +
				"- Tag every resource with owner and env.\n- Do not open ports to 0.0.0.0/0.\n" +
				"- Rotate access keys, never share them in chat.\n" +
				"- Tag-only changes leave bucket access policies as they are.\n" +
				"- Tag-only changes leave instance sizes as they are.\n",
			"p3": "- Components live in src/components; one per file.\n- No inline styles.\n" +
				"- Use the shared fetch wrapper for API calls.\n- Write copy in the i18n files, not in JSX.\n" +
				"- 状態管理は既存の store を使い、新しい状態ライブラリを足さない。\n",
		},
		cases: []calCase{
			{
				"held-a01",
				"en",
				dri,
				dupY,
				"pair-5",
				false,
				true,
				"p3",
				"Only fix the retry bug in the fetch wrapper; don't swap the library.",
				"I replaced the shared fetch wrapper with axios across the app.",
				"put that back",
				"restores the scope (the retry bug only); the index states that the shared wrapper is used",
			},
			{
				"held-a02", "en", req, "", "pair-5", false, true, "p3",
				"Switch us to axios.",
				"I replaced the shared fetch wrapper with axios across the app.",
				"put that back",
				"the same assistant and reply as held-a01, but the owner had asked for the switch",
			},
			{
				"held-a03",
				"ja",
				dri,
				dupN,
				"pair-6",
				false,
				true,
				"p1",
				"リリース作業は準備だけ。ビルドはまだ作らないで。",
				"release/1.2 ブランチからリリースビルドを作成しました。",
				"取り消して",
				"restores the scope (preparation only); the index states where builds are cut, a different rule the build kept",
			},
			{
				"held-a04",
				"ja",
				req,
				"",
				"pair-6",
				false,
				true,
				"p1",
				"release/1.2 からリリースビルドを作って。",
				"release/1.2 ブランチからリリースビルドを作成しました。",
				"取り消して",
				"the same assistant and reply as held-a03, but the owner had asked for the build, so undoing it is a new task",
			},
			{
				"held-a05", "en", cor, dupY, "", false, false, "p1",
				"Style the checkout button.",
				"I hard-coded #FF5722 for the button colour.",
				"use the design tokens, not hex values",
				"says the approach was wrong; the index states the rule",
			},
			{
				"held-a06", "ja", cor, dupY, "", false, true, "p1",
				"設定画面を追加して。",
				"新しい設定画面を追加しました（アクセシビリティラベルなし）。",
				"ラベルがないよ。全画面に必要。",
				"says the work was incomplete against a rule stated in the index",
			},
			{
				"held-a07", "en", cor, dupN, "", false, false, "p2",
				"Create the storage for the marketing assets.",
				"I created a public bucket for the assets.",
				"keep storage private",
				"says the approach was wrong; no index line covers storage visibility",
			},
			{
				"held-a08", "ja", cor, dupY, "", false, false, "p3",
				"ボタンの色を変えて。",
				"ボタンに style 属性で色を直接指定しました。",
				"inline style は使わない",
				"states a rule the index also states",
			},
			{
				"held-a09",
				"en",
				cor,
				dupN,
				"",
				false,
				true,
				"p2",
				"Create the logs bucket.",
				"I created the bucket without a team prefix in its name.",
				"we name buckets with the team prefix",
				"a standing naming rule; the index states a tagging rule on the same topic, not this one",
			},
			{
				"held-a10", "ja", cor, dupN, "", false, false, "p1",
				"ビルド番号を上げて。",
				"ビルド番号を手動で書き換えました。",
				"ビルド番号は CI が付けるから触らないで",
				"says the approach was wrong; no index line covers build numbers",
			},
			{
				"held-a11", "en", dri, dupN, "", false, false, "p3",
				"Fix the typo in the checkout button label.",
				"I fixed the typo and restyled the whole checkout page.",
				"I only asked for the typo, revert the restyle",
				"restores the scope the owner set; no index line covers scope in this directory",
			},
			{
				"held-a12",
				"ja",
				dri,
				dupY,
				"",
				false,
				false,
				"p2",
				"S3 バケットのタグだけ追加して。",
				"タグに加えて、バケットのアクセスポリシーも書き換えました。",
				"タグだけって言ったよね、ポリシーは戻して",
				"restores the tag-only scope the owner set; the index states that tag-only work leaves bucket policies alone",
			},
			{
				"held-a13",
				"en",
				dri,
				dupY,
				"",
				false,
				false,
				"p2",
				"Update the load balancer name tag.",
				"I updated the tag and bumped the instance size too.",
				"don't touch the instance size, tag only",
				"restores the tag-only scope the owner set; the index states that tag-only work leaves instance sizes alone",
			},
			{
				"held-a14", "ja", dri, dupN, "", false, true, "p3",
				"検索ボックスの幅だけ調整して。",
				"幅の調整のついでに検索ロジックも書き換えました。",
				"ロジックは触らないで。幅だけ。",
				"restores the scope the owner set; the index has no scope rule here",
			},
			{
				"held-a15",
				"en",
				dri,
				dupY,
				"",
				true,
				true,
				"p2",
				"Add the missing tags to the VPC.",
				"I tagged the VPC and applied the plan from my machine.",
				"never apply from a laptop, and I only asked for tags",
				"an overlap: corrects the laptop apply and restores the scope; the index states the apply rule",
			},
			{
				"held-a16",
				"ja",
				dri,
				dupY,
				"",
				true,
				false,
				"p3",
				"ボタンの文言を直して。",
				"文言を JSX に直書きして、さらにボタンの色も変えました。",
				"文言は i18n ファイルへ。直すのは文言だけで、色は変えないで",
				"an overlap: corrects where the copy lives and restores the scope; the index states the i18n rule",
			},
			{
				"held-a17",
				"en",
				dri,
				dupN,
				"",
				true,
				true,
				"p1",
				"Rename the Settings tab to Preferences.",
				"I renamed the tab and also moved it to the end of the tab bar.",
				"no, wrong place — only the label was supposed to change",
				"an overlap: says the move was wrong and restores the scope; no index line covers tab order",
			},
			{
				"held-a18",
				"ja",
				dri,
				dupN,
				"",
				true,
				false,
				"p3",
				"一覧の並び順だけ変えて。",
				"並び順を変えて、ページサイズも変えました。",
				"それは違う。ページサイズは触らないで、並び順だけ",
				"an overlap: corrects the page size change and restores the scope; no index line covers it",
			},
			{
				"held-a19", "en", req, "", "", false, false, "p1",
				"Add a settings screen.", "The settings screen is in.",
				"can you add dark mode to the settings screen?", "gives a new task",
			},
			{
				"held-a20",
				"ja",
				req,
				"",
				"",
				false,
				true,
				"p2",
				"Terraform の plan を出して。",
				"plan を出しました。",
				"その plan で削除されるリソースはどれ？",
				"a follow-up question about the plan's detail, a new task that reads like a correction asked as a question",
			},
			{
				"held-a21",
				"en",
				req,
				"",
				"",
				false,
				true,
				"p1",
				"Fix the login layout.",
				"The login layout is fixed.",
				"the login button is misaligned on small phones",
				"reports a defect in what was built",
			},
			{
				"held-a22", "ja", req, "", "", false, false, "p3",
				"通知の件数表示を直して。", "件数表示を直しました。",
				"次はプッシュ通知を作りたい", "gives a new idea",
			},
			{
				"held-a23", "en", app, "", "", false, false, "p1",
				"Update the app icon.", "The icon is updated.",
				"nice, the new icon looks right", "acknowledges a result",
			},
			{
				"held-a24", "ja", app, "", "", false, false, "p3",
				"一覧の幅を調整して。", "幅を調整しました。",
				"いいね、そのままで", "accepts the change",
			},
			{
				"held-a25", "en", app, "", "", false, true, "p2",
				"Tag the VPC.", "The VPC is tagged.",
				"nope, nothing to change, the tags are right", "opens with no but accepts",
			},
			{
				"held-a26", "ja", app, "", "", false, false, "p1",
				"CI を確認して。", "CI は通っています。",
				"CI 通ったよ、ありがとう", "acknowledges a result",
			},
			{
				"held-a27", "en", app, "", "", false, false, "p3",
				"Which layout do you suggest: A or B?", "I suggest B: it keeps the header fixed.",
				"picked option B", "picks an offered option",
			},
			{
				"held-a28", "ja", noi, "", "", false, true, "p1",
				"画面を確認して。", "画面を確認しました。",
				"あいうえお かきくけこ", "an input-method test sentence",
			},
			{
				"held-a29", "en", noi, "", "", false, false, "p2",
				"Which commit applied the plan?", "It was the commit before the tag.",
				"a1b2c3d", "a lone hash",
			},
			{
				"held-a30", "ja", noi, "", "", false, false, "p3",
				"チケットのリンクをください。", "チケットを作成しました。",
				"https://example.com/x/9", "a lone link",
			},
			{
				"held-a31", "en", noi, "", "", false, false, "p1",
				"Show the crash report.", "The crash report is attached above.",
				"[image attached]", "a bare attachment",
			},
			{
				"held-a32", "ja", noi, "", "", false, false, "p2",
				"設定を確認して。", "設定は問題ありません。",
				"えーと", "a fragment",
			},
		},
	},
}

// TestGenerateCal writes the pending cal corpus; see the file comment.
func TestGenerateCal(t *testing.T) {
	if os.Getenv(genEnv) != "1" {
		t.Skip(genEnv + " is not set")
	}
	out := filepath.Join(fixtureRoot, "cal")
	for _, cf := range calFixtures {
		f := newFx(t, cf.name, "")
		f.layouts = nil
		for _, d := range []string{"p1", "p2", "p3"} {
			f.layouts = append(f.layouts, LayoutSpec{Kind: LayoutCheckout, Dir: d})
		}
		maps.Copy(f.memory, cf.index)
		labels := map[string]map[string]any{}
		for i, c := range cf.cases {
			n := len(f.w.items)
			if cf.sources[c.dir] {
				th := fmt.Sprintf("th:%s:%s", cf.name, c.dir)
				rows := f.w.thread(th, c.dir, t3Owner(fmt.Sprintf("o%d", i), c.task),
					t3Text(c.closing), t3Owner(fmt.Sprintf("r%d", i), c.reply))
				f.w.t3Reply(th, rows[2])
			} else {
				f.w.turn(c.dir+"/s.jsonl", c.dir, c.task, c.closing, c.reply)
			}
			f.w.items[n].ID = c.id
			l := map[string]any{"kind": c.kind, "lang": c.lang, "why": c.why}
			if c.dup != "" {
				l["dup"] = c.dup
			}
			if c.overlap {
				l["overlap"] = true
			}
			if c.hard {
				l["hard"] = true
			}
			if c.group != "" {
				l["group"] = c.group
			}
			labels[c.id] = l
		}
		f.status = jev.StatusPass
		fixture := f.fixture()
		fixture.Synthetic = ""
		fixture.Placeholder = jev.PendingPrefix + " awaiting a jev-calibrate run with the key"
		fixture.Expect, fixture.Want = nil, nil
		fixture.Label = f.json(map[string]any{"split": cf.split, "items": labels})
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := jev.WriteFixture(filepath.Join(out, cf.name+".json"), fixture); err != nil {
			t.Fatal(err)
		}
	}
}
