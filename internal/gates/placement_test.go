package gates

import "testing/fstest"

var placementCases = []checkCase{
	{
		name: "lines_replaced_outside_test_functions_pass",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func helper() {\n\t_ = 2\n\t_ = 3\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(withTest("func helper() {\n\t_ = 1\n\t_ = 4\n}\n"))},
	},
	{
		name: "lines_replaced_inside_a_test_function_pass",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 2\n\t_ = 3\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n\t_ = 4\n}\n")},
	},
	{
		name: "new_test_function_offset_by_deleted_test_lines_pass",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n}\n\nfunc TestY() {\n\t_ = 9\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file(tests(5))},
	},
	{
		name: "gofmt_realignment_in_a_struct_adds_nothing",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\ntype fake struct {\n\tname   string\n\tcalled int\n}\n\nfunc TestX() {\n\t_ = 1\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\ntype fake struct {\n\tname string\n\tcalled int\n}\n\nfunc TestX() {\n\t_ = 1\n}\n")},
	},
	{
		name: "gofmt_realignment_in_a_test_function_adds_nothing",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = []struct{ a, b int }{\n\t\t{1,   2},\n\t}\n\tx :=   1\n\t_ = x\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = []struct{ a, b int }{\n\t\t{1, 2},\n\t}\n\tx := 1\n\t_ = x\n}\n")},
	},
	{
		name: "lines_added_to_a_helper_pass",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func helper() {\n\t_ = 1\n\t_ = 2\n\t_ = 3\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(withTest("func helper() {\n\t_ = 1\n}\n"))},
	},
	{
		name: "fields_added_to_a_type_pass",
		head: fstest.MapFS{"a/a_test.go": file(withTest("type fake struct {\n\ta int\n\tb int\n\tc int\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(withTest("type fake struct {\n\ta int\n}\n"))},
	},
	{
		name: "rows_added_to_a_package_level_table_fail",
		head: fstest.MapFS{"a/a_test.go": file(withTest("var rows = []int{\n\t1,\n\t2,\n\t3,\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(withTest("var rows = []int{\n\t1,\n}\n"))},
		want: []string{"a/a_test.go:contract_tests"},
	},
	{
		name: "package_level_table_rows_replaced_pass",
		head: fstest.MapFS{"a/a_test.go": file(withTest("var rows = []int{\n\t2,\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(withTest("var rows = []int{\n\t1,\n}\n"))},
	},
	{
		name: "helper_named_like_a_test_but_lowercase_passes",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func Testify() {\n\t_ = 1\n\t_ = 2\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(tests(2))},
	},
	{
		name: "method_named_like_a_test_passes",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func (s suite) TestY() {\n\t_ = 1\n\t_ = 2\n}\n\ntype suite struct{}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(tests(2))},
	},
	{
		name: "line_added_to_an_existing_test_body_fails",
		head: fstest.MapFS{"a/a_test.go": file(tests(5))},
		base: fstest.MapFS{"a/a_test.go": file(tests(4))},
		want: []string{"a/a_test.go:contract_tests"},
	},
	{
		name: "new_test_function_fails",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func TestY() {\n\t_ = 9\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(tests(2))},
		want: []string{"a/a_test.go:contract_tests"},
	},
	{
		name: "new_copy_of_a_test_function_fails",
		head: fstest.MapFS{"a/a_test.go": file(withTest("func TestY() {\n\t_ = 1\n\t_ = 1\n}\n"))},
		base: fstest.MapFS{"a/a_test.go": file(tests(2))},
		want: []string{"a/a_test.go:contract_tests"},
	},
	{
		name: "new_benchmark_fuzz_and_example_functions_fail",
		head: fstest.MapFS{
			"a/a_test.go": file(withTest("func BenchmarkY() {\n\t_ = 9\n}\n")),
			"a/b_test.go": file("package a\n\nfunc FuzzY() {\n\t_ = 9\n}\n"),
			"a/c_test.go": file("package a\n\nfunc Example() {\n\t_ = 9\n}\n"),
		},
		base: fstest.MapFS{"a/a_test.go": file(tests(2))},
		want: []string{"a/a_test.go:contract_tests", "a/b_test.go:contract_tests", "a/c_test.go:contract_tests"},
	},
	{
		name: "comment_lines_in_a_test_function_add_nothing",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t// why\n\t// because\n" + testBody(6) + "}\n")},
		base: fstest.MapFS{"a/a_test.go": file(tests(6))},
	},
	{
		name: "code_after_a_block_comment_on_the_same_line_counts",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n\t/* setup */ _ = 2\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n}\n")},
		want: []string{"a/a_test.go:contract_tests"},
	},
	{
		name: "continuation_lines_of_a_block_comment_after_code_add_nothing",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n" + testBody(10) + "\t_ = 1 /* a\n\tb\n\tc */\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n" + testBody(10) + "\t_ = 1\n}\n")},
	},
	{
		name: "helper_line_moved_into_a_test_function_fails",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n\t_ = 7\n}\n")},
		base: fstest.MapFS{"a/a_test.go": file("package a\n\nfunc TestX() {\n\t_ = 1\n}\n\nfunc helper() {\n\t_ = 7\n}\n")},
		want: []string{"a/a_test.go:contract_tests"},
	}}
