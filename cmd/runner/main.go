package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/apenella/go-ansible/v2/pkg/execute"
	"github.com/apenella/go-ansible/v2/pkg/execute/stdoutcallback"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
)

func main() {
	var inventoryPath, playbookPath, resultPath string
	flag.StringVar(&inventoryPath, "i", "", "inventory path")
	flag.StringVar(&playbookPath, "p", "", "playbook yaml path")
	flag.StringVar(&resultPath, "o", "", "store result path")
	flag.Parse()

	var err error
	buff := new(bytes.Buffer)

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()

	ansiblePlaybookOptions := &playbook.AnsiblePlaybookOptions{
		Verbose: false,
		Become:  false,
		ExtraVars: map[string]interface{}{
			"ansible_ssh_private_key_file": "/root/.ssh/id_rsa",
			"ansible_user":                 "auser",
			"ansible_port":                 8513,
		},
		Inventory:     inventoryPath,
		SSHCommonArgs: "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null",
		User:          "auser",
	}

	cmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(playbookPath),
		playbook.WithPlaybookOptions(ansiblePlaybookOptions),
	)
	fmt.Println(cmd.String())

	env := map[string]string{"ANSIBLE_STDOUT_CALLBACK": "json"}

	exec := stdoutcallback.NewJSONStdoutCallbackExecute(
		//exec := stdoutcallback.NewDebugStdoutCallbackExecute(
		execute.NewDefaultExecute(
			execute.WithEnvVars(env),
			execute.WithCmd(cmd),
			execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
			execute.WithWrite(io.Writer(buff)),
		),
	)

	err = exec.Execute(ctx)
	if err != nil {
		log.Fatal(err)
	}

	body, err := io.ReadAll(io.Reader(buff))
	fmt.Println(string(body))

	if err != nil {
		log.Fatal(err)
	}

	if err := os.WriteFile(resultPath, body, 0644); err != nil {
		log.Fatal(err)
	}

}
