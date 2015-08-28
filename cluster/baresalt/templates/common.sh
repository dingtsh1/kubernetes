#!/bin/bash

# Copyright 2014 The Kubernetes Authors All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Install salt from GCS.  See README.md for instructions on how to update these
# debs.
#
# $1 If set to --master, also install the master
install-salt() {
  curl --insecure -L https://bootstrap.saltstack.com -o install_salt.sh

  chmod +x install_salt.sh

  if [[ ${1-} == '--master' ]]; then
    ./install_salt.sh -M
  else
    ./install_salt.sh
  fi
}
