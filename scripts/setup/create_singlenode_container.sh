#!/usr/bin/env bash
SERVER=$1

DIR="$(pwd)/scripts/setup/"

source "$(pwd)/scripts/setup/setup.cfg"

server_exec() { 
    ssh -oStrictHostKeyChecking=no -p 22 "$SERVER" $1; 
}

{
    # Set up up vHive in the container mode.
    server_exec 'sudo DEBIAN_FRONTEND=noninteractive apt-get autoremove' 
    server_exec "git clone --branch=$VHIVE_BRANCH https://github.com/ease-lab/vhive"

    # Setup github authentication.
    ACCESS_TOKEH="$(cat $GITHUB_TOKEN)"
    server_exec 'echo -en "\n\n" | ssh-keygen -t rsa'
    server_exec 'ssh-keyscan -t rsa github.com >> ~/.ssh/known_hosts'
    server_exec 'curl -H "Authorization: token '"$ACCESS_TOKEH"'" --data "{\"title\":\"'"key:\$(hostname)"'\",\"key\":\"'"\$(cat ~/.ssh/id_rsa.pub)"'\"}" https://api.github.com/user/keys'
    # server_exec 'sleep 5'

    # Get loader and dependencies.
    server_exec "git clone --branch=$LOADER_BRANCH git@github.com:eth-easl/loader.git"
    server_exec 'echo -en "\n\n" | sudo apt-get install python3-pip python-dev'
    server_exec 'cd; cd loader; pip install -r config/requirements.txt'

    # Install YQ and rewrite the YAML files for Knative and Istio in the vHive repo
    server_exec 'sudo wget https://github.com/mikefarah/yq/releases/download/v4.27.3/yq_linux_amd64 -O /usr/bin/yq && sudo chmod +x /usr/bin/yq'
    server_exec 'cd; ./loader/scripts/setup/rewrite_yaml_files.sh'

    server_exec 'cd vhive; ./scripts/cloudlab/setup_node.sh stock-only'
    server_exec 'tmux new -s runner -d'
    server_exec 'tmux new -s kwatch -d'
    server_exec 'tmux new -d -s containerd'
    server_exec 'tmux new -d -s cluster'
    server_exec 'tmux send-keys -t containerd "sudo containerd" ENTER'
    sleep 3s
    server_exec 'cd vhive; ./scripts/cluster/create_one_node_cluster.sh stock-only'
    server_exec 'tmux send-keys -t cluster "watch -n 0.5 kubectl get pods -A" ENTER'

    # Update Golang.
    server_exec 'wget -q https://dl.google.com/go/go1.17.linux-amd64.tar.gz'
    server_exec 'sudo rm -rf /usr/local/go && sudo tar -C /usr/local/ -xzf go1.17.linux-amd64.tar.gz'
    server_exec 'rm go1.17*'
    server_exec 'echo "export PATH=$PATH:/usr/local/go/bin" >> ~/.profile'
    server_exec 'source ~/.profile'

    $DIR/expose_infra_metrics.sh $SERVER

    #* Disable turbo boost.
    server_exec './vhive/scripts/utils/turbo_boost.sh disable'
    #* Disable hyperthreading.
    server_exec 'echo off | sudo tee /sys/devices/system/cpu/smt/control'
    #* Create CGroup.
    server_exec 'sudo bash loader/scripts/isolation/define_cgroup.sh'

    echo "Logging in master node $SEVER"
    exit
}
